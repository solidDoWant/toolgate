package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// login starts a sign-in (docs/design/phase-2.md §5, step 1): it stores the
// state, nonce and PKCE verifier in ui_logins, sets the per-sign-in state
// cookie and redirects to the IdP.
func (u *UI) login(writer http.ResponseWriter, request *http.Request) {
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

	err = u.queries.InsertUILogin(ctx, db.InsertUILoginParams{
		StateHash:    stateHash,
		Nonce:        nonce,
		PkceVerifier: verifier,
		ReturnTo:     ReturnTo(request.URL.Query().Get("return_to")),
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
		u.oauthConfig(provider).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)),
		http.StatusSeeOther)
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

func loginCookieName(stateHash []byte) string {
	return LoginCookiePrefix + hex.EncodeToString(stateHash)[:8]
}

// signInFailure is a failed sign-in: its reason goes into the auth.refuse
// event and the page.
type signInFailure struct {
	reason string
	err    error
}

func (f *signInFailure) Error() string { return f.reason }

func failure(reason string, err error) *signInFailure {
	return &signInFailure{reason: reason, err: err}
}

// callback completes a sign-in (docs/design/phase-2.md §5, steps 2–5).
func (u *UI) callback(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")

	returnTo, failed := u.completeSignIn(writer, request)
	if failed != nil {
		u.refuse(request, failed)
		http.Error(writer, "Sign-in failed: "+failed.reason+". Start again at "+LoginPath+".", http.StatusForbidden)

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
		return "", failure("the state does not match this browser's sign-in", nil)
	}

	login, err := u.queries.TakeUILogin(ctx, stateHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", failure("the sign-in is unknown, already used or expired", nil)
	}

	if err != nil {
		return "", failure("the gateway can't load the sign-in", err)
	}

	if idpError := query.Get("error"); idpError != "" {
		return "", failure("the identity provider returned an error", errors.New(truncate(idpError)))
	}

	provider, err := u.oidcProvider(ctx)
	if err != nil {
		return "", failure("the gateway can't reach its identity provider", err)
	}

	clientCtx := u.clientContext(ctx)

	token, err := u.oauthConfig(provider).Exchange(clientCtx, query.Get("code"), oauth2.VerifierOption(login.PkceVerifier))
	if err != nil {
		return "", failure("the code exchange failed", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return "", failure("the identity provider returned no ID token", nil)
	}

	idToken, err := provider.VerifierContext(clientCtx, &oidc.Config{ClientID: u.options.ClientID}).Verify(clientCtx, rawIDToken)
	if err != nil {
		return "", failure("the ID token is not valid", err)
	}

	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(login.Nonce)) != 1 {
		return "", failure("the ID token's nonce does not match", nil)
	}

	identity, err := u.identityFromIDToken(idToken)
	if err != nil {
		return "", failure("the ID token's claims are not valid", err)
	}

	if login.StepUp {
		// A fresh sign-in for a grant or an approval has its own checks
		// (docs/design/phase-2.md §5, "Step-up"). Until they run here, a
		// step-up sign-in fails closed.
		return "", failure("a step-up sign-in can't be completed", nil)
	}

	if failed := u.startSession(writer, request, identity); failed != nil {
		return "", failed
	}

	return login.ReturnTo, nil
}

// signedInIdentity is what a sign-in learned from the ID token.
type signedInIdentity struct {
	principal string
	email     string
	name      string
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
