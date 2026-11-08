package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// login starts a sign-in that returns to the return_to parameter and, when
// step_up is 1, asks the IdP for a fresh sign-in.
func (u *UI) login(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	u.beginSignIn(writer, request, ReturnTo(query.Get("return_to")), query.Get("step_up") == "1", false)
}

// beginSignIn stores the state, nonce and PKCE verifier in ui_logins, sets
// the per-sign-in state cookie and redirects to the IdP. restarted marks a
// sign-in the gateway started again after a failed callback, so it is
// restarted at most once.
func (u *UI) beginSignIn(writer http.ResponseWriter, request *http.Request, returnTo string, stepUp, restarted bool) {
	ctx := request.Context()

	pending, err := u.queries.CountUILogins(ctx)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot count the sign-ins in progress", "error", err)
		http.Error(writer, "The gateway can't start a sign-in now. Try again later.", http.StatusInternalServerError)

		return
	}

	if pending > MaxPendingLogins {
		http.Error(writer, "Too many sign-ins are in progress. Try again in a few minutes.", http.StatusServiceUnavailable)

		return
	}

	provider, err := u.oidcProvider(ctx)
	if err != nil {
		u.logger.WarnContext(ctx, "cannot reach the identity provider", "error", err)
		http.Error(writer, "The gateway can't reach its identity provider. Try again later.", http.StatusServiceUnavailable)

		return
	}

	state := randomValue()
	nonce := randomValue()
	verifier := oauth2.GenerateVerifier()
	stateHash := hashValue(state)

	authOptions := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)}

	// A step-up records the browser's session, so the callback updates it,
	// and asks the IdP for a fresh sign-in.
	var sessionHash []byte

	if stepUp {
		// max_age is the clock skew, not 0: some IdPs read max_age=0 as "no
		// limit" (Authentik tests it for truth), and Authentik honours
		// prompt=login only once per browser session. A max_age of 60 seconds
		// still forces a new sign-in there, and checkStepUpAuthTime accepts an
		// auth_time up to that skew before the sign-in started.
		authOptions = append(authOptions,
			oauth2.SetAuthURLParam("prompt", "login"),
			oauth2.SetAuthURLParam("max_age", strconv.Itoa(int(StepUpClockSkew/time.Second))))

		sessionHash, err = u.sessionHash(request)
		if err != nil {
			u.logger.ErrorContext(ctx, "cannot load the web UI session", "error", err)
			http.Error(writer, "The gateway can't start a sign-in now. Try again later.", http.StatusInternalServerError)

			return
		}
	}

	err = u.queries.InsertUILogin(ctx, db.InsertUILoginParams{
		StateHash:     stateHash,
		Nonce:         nonce,
		PkceVerifier:  verifier,
		ReturnTo:      returnTo,
		StepUp:        stepUp,
		UiSessionHash: sessionHash,
		Restarted:     restarted,
	})
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot store the sign-in", "error", err)
		http.Error(writer, "The gateway can't start a sign-in now. Try again later.", http.StatusInternalServerError)

		return
	}

	http.SetCookie(writer, &http.Cookie{
		Name:     loginCookieName(stateHash),
		Value:    state,
		Path:     "/",
		MaxAge:   int(loginLifetime / time.Second),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writer.Header().Set("Cache-Control", "no-store")
	http.Redirect(writer, request,
		u.oauthConfig(provider).AuthCodeURL(state, authOptions...),
		http.StatusSeeOther)
}

// sessionHash returns the ID hash of the request's valid UI session, or nil
// when it has none.
func (u *UI) sessionHash(request *http.Request) ([]byte, error) {
	if _, err := u.loadSession(request); errors.Is(err, errNoSession) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	cookie, _ := request.Cookie(SessionCookie) // loadSession found it.

	return hashValue(cookie.Value), nil
}

// ReturnTo returns where a sign-in goes when it ends: raw when it is a path
// under `/ui/`, else HomePath, so a sign-in never redirects off the gateway.
func ReturnTo(raw string) string {
	if len(raw) > maxReturnTo || !strings.HasPrefix(raw, "/ui/") || strings.ContainsAny(raw, "\\\x00\r\n\t") {
		return HomePath
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil ||
		!strings.HasPrefix(parsed.Path, "/ui/") {
		return HomePath
	}

	return raw
}

// The page of a restarted sign-in whose state again does not match this
// browser's sign-in.
const (
	stateMismatchHappened = "Your browser didn't keep the cookie of this sign-in, so toolgate can't finish it."
	stateMismatchToDo     = "Allow cookies for this site, sign in from one tab only, then sign in again."
)

// The page of any other failed sign-in. The technical reason is in the
// audit event, not on the page.
const (
	signInFailedHappened = "toolgate couldn't finish your sign-in."
	signInFailedToDo     = "Sign in again. If it fails again, ask your admin to look at the gateway's audit log."
)

// signInFailedPage is the content of the page of a failed sign-in.
type signInFailedPage struct {
	Happened string
	ToDo     string
	Retry    string
}

// renderSignInFailure writes the page of a failed sign-in in the web UI
// layout, with a button that starts a new sign-in.
func (u *UI) renderSignInFailure(writer http.ResponseWriter, request *http.Request, failed *signInFailure) {
	content := signInFailedPage{Happened: failed.happened, ToDo: failed.todo, Retry: failed.retry}
	if content.Happened == "" {
		content.Happened, content.ToDo = signInFailedHappened, signInFailedToDo
	}

	if content.Retry == "" {
		content.Retry = LoginPath
	}

	u.writePage(writer, request, http.StatusForbidden, "signin_failed",
		page{Title: "Sign-in failed", SignedOut: true, Data: content})
}

// signInURL is the sign-in that returns to returnTo, as a step-up when
// stepUp is set.
func signInURL(returnTo string, stepUp bool) string {
	if stepUp {
		return StepUpURL(returnTo)
	}

	return LoginPath + "?" + url.Values{"return_to": {ReturnTo(returnTo)}}.Encode()
}

func loginCookieName(stateHash []byte) string {
	return LoginCookiePrefix + hex.EncodeToString(stateHash)[:8]
}

// signInFailure is a failed sign-in: its reason goes into the auth.refuse
// event, and happened and todo onto the page.
type signInFailure struct {
	reason string
	// happened says in plain words what went wrong, and todo what the user
	// can do. Empty fields get a general text.
	happened string
	todo     string
	// retry is where the page's "Sign in again" button goes.
	retry string
	err   error
	// restart, if set, replaces the page with a new sign-in.
	restart *signInRestart
}

// signInRestart is the new sign-in that follows a failure a new sign-in
// can fix.
type signInRestart struct {
	// known is false when the failed sign-in's row is gone: the browser then
	// starts a plain sign-in at LoginPath.
	known    bool
	returnTo string
	stepUp   bool
}

func (f *signInFailure) Error() string { return f.reason }

func failure(reason string, err error) *signInFailure {
	return &signInFailure{reason: reason, err: err}
}

// callback completes a sign-in.
func (u *UI) callback(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")

	returnTo, failed := u.completeSignIn(writer, request)
	if failed != nil {
		u.refuse(request, failed)

		if failed.restart != nil {
			u.restartSignIn(writer, request, *failed.restart)

			return
		}

		u.renderSignInFailure(writer, request, failed)

		return
	}

	http.Redirect(writer, request, returnTo, http.StatusSeeOther)
}

func (u *UI) completeSignIn(writer http.ResponseWriter, request *http.Request) (string, *signInFailure) {
	ctx := request.Context()
	query := request.URL.Query()

	state := query.Get("state")
	if state == "" {
		return "", failure("the callback has no state", nil)
	}

	stateHash := hashValue(state)
	cookieName := loginCookieName(stateHash)

	// The state cookie works once, whatever the outcome.
	http.SetCookie(writer, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})

	// The state must equal this browser's cookie, so a callback started in
	// another browser is refused.
	cookie, err := request.Cookie(cookieName)
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		return "", u.stateMismatch(ctx, stateHash)
	}

	login, err := u.queries.TakeUILogin(ctx, stateHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", failure("the sign-in is unknown, already used or expired", nil)
	}

	if err != nil {
		return "", failure("the gateway can't load the sign-in", err)
	}

	if failed := u.finishSignIn(writer, request, login); failed != nil {
		if failed.retry == "" {
			failed.retry = signInURL(login.ReturnTo, login.StepUp)
		}

		return "", failed
	}

	return login.ReturnTo, nil
}

// finishSignIn completes the sign-in of a matching state: it checks the
// IdP's answer and starts or updates the session.
func (u *UI) finishSignIn(writer http.ResponseWriter, request *http.Request, login db.UiLogin) *signInFailure {
	ctx := request.Context()
	query := request.URL.Query()

	if idpError := query.Get("error"); idpError != "" {
		return failure("the identity provider returned an error", errors.New(truncate(idpError)))
	}

	provider, err := u.oidcProvider(ctx)
	if err != nil {
		return failure("the gateway can't reach its identity provider", err)
	}

	clientCtx := u.clientContext(ctx)

	token, err := u.oauthConfig(provider).Exchange(clientCtx, query.Get("code"), oauth2.VerifierOption(login.PkceVerifier))
	if err != nil {
		return failure("the code exchange failed", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return failure("the identity provider returned no ID token", nil)
	}

	idToken, err := provider.VerifierContext(clientCtx, &oidc.Config{ClientID: u.options.ClientID}).Verify(clientCtx, rawIDToken)
	if err != nil {
		return failure("the ID token is not valid", err)
	}

	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(login.Nonce)) != 1 {
		return failure("the ID token's nonce does not match", nil)
	}

	identity, err := u.identityFromIDToken(idToken)
	if err != nil {
		return failure("the ID token's claims are not valid", err)
	}

	if login.StepUp {
		return u.completeStepUp(writer, request, login, identity)
	}

	return u.startSession(writer, request, identity)
}

// stateMismatch is the failure of a callback whose state is not this
// browser's sign-in: the cookie expired, the user went back to an old
// callback, or another browser started the sign-in. The old sign-in never
// completes, since the cookie check is what stops a callback from another
// browser signing this one in. A new sign-in from this browser keeps the old
// one's return path and step-up, unless it was itself a restart.
func (u *UI) stateMismatch(ctx context.Context, stateHash []byte) *signInFailure {
	failed := failure("the state does not match this browser's sign-in", nil)

	login, err := u.queries.TakeUILogin(ctx, stateHash)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		failed.restart = &signInRestart{}
	case err != nil:
		failed.err = err
	case login.Restarted:
		failed.happened, failed.todo = stateMismatchHappened, stateMismatchToDo
		failed.retry = signInURL(login.ReturnTo, login.StepUp)
	default:
		failed.restart = &signInRestart{known: true, returnTo: ReturnTo(login.ReturnTo), stepUp: login.StepUp}
	}

	return failed
}

// restartSignIn starts the new sign-in that follows a failed callback.
func (u *UI) restartSignIn(writer http.ResponseWriter, request *http.Request, restart signInRestart) {
	if !restart.known {
		http.Redirect(writer, request, LoginPath, http.StatusSeeOther)

		return
	}

	u.beginSignIn(writer, request, restart.returnTo, restart.stepUp, true)
}

// completeStepUp runs the step-up checks
// and records the fresh sign-in: on the session the step-up
// started from, or on a new session when there was none.
func (u *UI) completeStepUp(writer http.ResponseWriter, request *http.Request, login db.UiLogin, identity signedInIdentity) *signInFailure {
	ctx := request.Context()

	if !identity.authTime.Valid {
		return &signInFailure{reason: "the ID token has no auth_time", happened: missingAuthTimeHappened, todo: missingAuthTimeToDo}
	}

	if err := checkStepUpAuthTime(identity.authTime.Time, login.CreatedAt.Time, time.Now()); err != nil {
		failed := failure("the step-up sign-in is not fresh", err)
		if login.Restarted {
			failed.happened, failed.todo = notFreshHappened, notFreshToDo
		} else {
			failed.restart = &signInRestart{known: true, returnTo: ReturnTo(login.ReturnTo), stepUp: true}
		}

		return failed
	}

	if login.UiSessionHash == nil {
		return u.startSession(writer, request, identity)
	}

	session, err := u.queries.GetValidUISession(ctx, login.UiSessionHash)
	if errors.Is(err, pgx.ErrNoRows) {
		// The session ended while the user signed in again.
		return u.startSession(writer, request, identity)
	}

	if err != nil {
		return failure("the gateway can't load the session", err)
	}

	if session.Principal != identity.principal {
		if err := u.queries.DeleteUISession(ctx, login.UiSessionHash); err != nil {
			return failure("the gateway can't sign out the session", err)
		}

		http.SetCookie(writer, sessionCookie("", -1))

		return &signInFailure{reason: "the step-up signed in as a different user", happened: differentUserHappened, todo: differentUserToDo}
	}

	updated, err := u.queries.UpdateUISessionSignIn(ctx, db.UpdateUISessionSignInParams{
		Email:     identity.email,
		Name:      identity.name,
		Groups:    identity.groups,
		AuthTime:  identity.authTime,
		IDHash:    login.UiSessionHash,
		Principal: identity.principal,
	})
	if err != nil {
		return failure("the gateway can't update the session", err)
	}

	u.recordProfile(ctx, identity)

	if updated == 0 {
		return u.startSession(writer, request, identity)
	}

	return nil
}

// signedInIdentity is what a sign-in learned from the ID token.
type signedInIdentity struct {
	principal string
	email     string
	name      string
	username  string
	groups    []string
	authTime  pgtype.Timestamptz
}

// identityFromIDToken reads sub, the email (only when verified), the name,
// the groups and auth_time, with the configured claim names.
func (u *UI) identityFromIDToken(idToken *oidc.IDToken) (signedInIdentity, error) {
	if idToken.Subject == "" {
		return signedInIdentity{}, errors.New("no subject")
	}

	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return signedInIdentity{}, fmt.Errorf("claims: %w", err)
	}

	identity := signedInIdentity{principal: idToken.Subject, groups: []string{}}

	var verifiedEmail bool
	if raw, ok := claims[u.claims.emailVerified]; ok {
		_ = json.Unmarshal(raw, &verifiedEmail)
	}

	if raw, ok := claims[u.claims.email]; ok && verifiedEmail {
		if err := json.Unmarshal(raw, &identity.email); err != nil {
			return signedInIdentity{}, fmt.Errorf("email claim: %w", err)
		}
	}

	if raw, ok := claims["name"]; ok {
		_ = json.Unmarshal(raw, &identity.name)
	}

	if raw, ok := claims["preferred_username"]; ok {
		_ = json.Unmarshal(raw, &identity.username)
	}

	// Groups that aren't a list of strings are refused rather than ignored,
	// as for access tokens.
	if raw, ok := claims[u.claims.groups]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &identity.groups); err != nil {
			return signedInIdentity{}, fmt.Errorf("groups claim: %w", err)
		}
	}

	if raw, ok := claims["auth_time"]; ok {
		var seconds float64
		if err := json.Unmarshal(raw, &seconds); err != nil {
			return signedInIdentity{}, fmt.Errorf("auth_time claim: %w", err)
		}

		identity.authTime = pgtype.Timestamptz{Time: time.Unix(int64(seconds), 0), Valid: true}
	}

	return identity, nil
}

// startSession inserts a UI session, deletes the browser's old one and sets
// the session cookie.
func (u *UI) startSession(writer http.ResponseWriter, request *http.Request, identity signedInIdentity) *signInFailure {
	ctx := request.Context()
	sessionID := randomValue()

	csrfToken := make([]byte, randomBytes)
	_, _ = rand.Read(csrfToken) // crypto/rand.Read never fails.

	_, err := u.queries.InsertUISession(ctx, db.InsertUISessionParams{
		IDHash:    hashValue(sessionID),
		Principal: identity.principal,
		Email:     identity.email,
		Name:      identity.name,
		Groups:    identity.groups,
		CsrfToken: csrfToken,
		AuthTime:  identity.authTime,
	})
	if err != nil {
		return failure("the gateway can't store the session", err)
	}

	u.recordProfile(ctx, identity)

	if old, err := request.Cookie(SessionCookie); err == nil && old.Value != "" {
		if err := u.queries.DeleteUISession(ctx, hashValue(old.Value)); err != nil {
			u.logger.WarnContext(ctx, "cannot delete the browser's previous web UI session", "error", err)
		}
	}

	http.SetCookie(writer, sessionCookie(sessionID, int(SessionLifetime/time.Second)))

	return nil
}

func sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// logout deletes the session row and its cookie.
func (u *UI) logout(writer http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(SessionCookie); err == nil {
		if err := u.queries.DeleteUISession(request.Context(), hashValue(cookie.Value)); err != nil {
			u.logger.ErrorContext(request.Context(), "cannot delete the web UI session", "error", err)
			http.Error(writer, "The gateway can't sign you out now. Try again later.", http.StatusInternalServerError)

			return
		}
	}

	http.SetCookie(writer, sessionCookie("", -1))
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = writer.Write([]byte("You are signed out of toolgate.\n"))
}

// refuse writes the auth.refuse event of a failed sign-in, at most 10 a
// minute per remote address; the rest are counted in a metric.
func (u *UI) refuse(request *http.Request, failed *signInFailure) {
	if failed.err != nil {
		u.logger.WarnContext(request.Context(), "web UI sign-in failed", "reason", failed.reason, "error", failed.err)
	}

	refusal := auth.Refusal{Reason: "web UI sign-in: " + failed.reason, RemoteAddress: auth.RemoteIP(request)}

	if !u.limiter.Allow(refusal.RemoteAddress) {
		u.dropped.Inc()

		return
	}

	if u.options.Recorder != nil {
		u.options.Recorder.RecordAuthRefusal(request.Context(), refusal)
	}
}

const maxErrorField = 256

func truncate(value string) string {
	if len(value) > maxErrorField {
		return value[:maxErrorField]
	}

	return value
}

// recordProfile stores the verified display fields of a sign-in so pages can
// show the person instead of the raw subject. A failure only costs the
// friendly name, so it is logged and the sign-in goes on.
func (u *UI) recordProfile(ctx context.Context, identity signedInIdentity) {
	if identity.email == "" && identity.name == "" && identity.username == "" {
		return
	}

	err := u.queries.UpsertPrincipalProfile(ctx, db.UpsertPrincipalProfileParams{
		Principal: identity.principal, Email: identity.email, Name: identity.name, Username: identity.username,
	})
	if err != nil {
		u.logger.WarnContext(ctx, "cannot record the signed-in user's display name", "error", err)
	}
}
