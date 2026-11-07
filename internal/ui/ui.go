// Package ui serves the gateway's web UI under `/ui/`:
// browser sign-in with the IdP, UI sessions, and the route helper that
// gives every page an access level.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Paths and cookie names of the sign-in.
const (
	LoginPath    = "/ui/login"
	CallbackPath = "/ui/auth/callback"
	LogoutPath   = "/ui/logout"
	StaticPath   = "/ui/static/"
	// HomePath is where `GET /` and a sign-in without a valid return_to go.
	HomePath = "/ui/servers"
	// RestartPath requests the restart of an outdated instance.
	RestartPath = "/ui/instances/{id}/restart"

	// SessionCookie holds the UI session's random ID.
	SessionCookie = "__Host-toolgate"
	// LoginCookiePrefix starts the name of the cookie that holds one
	// sign-in's state. The first 8 hex digits of the state's SHA-256 follow.
	LoginCookiePrefix = "__Host-toolgate-login-"
)

// Time bounds and limits.
const (
	SessionLifetime = 8 * time.Hour
	loginLifetime   = 10 * time.Minute
	touchInterval   = time.Minute

	// MaxPendingLogins is the number of sign-ins in progress above which
	// `/ui/login` is refused.
	MaxPendingLogins = 10000

	// maxReturnTo bounds the return_to path stored with a sign-in.
	maxReturnTo = 2048

	// randomBytes is the size of every random value: session IDs, CSRF
	// tokens, states and nonces.
	randomBytes = 32

	discoveryTimeout = 10 * time.Second
)

// Access is the access level of a UI route.
type Access int

// Access levels.
const (
	// Public routes need no session. Only the sign-in routes and the static
	// files are public.
	Public Access = iota
	// User routes need a valid UI session.
	User
	// Admin routes need a valid UI session whose groups contain the admin
	// group.
	Admin
)

// String names the access level.
func (a Access) String() string {
	switch a {
	case Public:
		return "public"
	case User:
		return "user"
	case Admin:
		return "admin"
	default:
		return fmt.Sprintf("Access(%d)", int(a))
	}
}

// Registrar is where the UI registers its routes: the gateway's main port.
type Registrar interface {
	// HandlePublic registers a route served without authentication.
	HandlePublic(pattern string, handler http.Handler)
	// Handle registers a route whose handler authenticates the request.
	Handle(pattern string, handler http.Handler)
}

// Route is a registered UI route and its access level.
type Route struct {
	Pattern string
	Access  Access
}

// Options configures the UI.
type Options struct {
	// Database holds the UI sessions and sign-ins in progress.
	Database db.DBTX
	// IssuerURL is the IdP's issuer (`oidcIssuerURL`).
	IssuerURL string
	// ClientID and ClientSecret are the UI's OIDC client. An empty secret
	// makes it a public client.
	ClientID     string
	ClientSecret string
	// Scopes are requested at sign-in.
	Scopes []string
	// PublicURL is the gateway's URL in the browser, without a trailing
	// slash. The redirect URI is PublicURL + CallbackPath.
	PublicURL string
	// AdminGroup names the admins. Empty means nobody is an admin.
	AdminGroup string
	// Claim names, as for access tokens. Empty names take the defaults
	// `email`, `email_verified` and `groups`.
	EmailClaim         string
	EmailVerifiedClaim string
	GroupsClaim        string
	// Recorder receives failed sign-ins. Nil records nothing.
	Recorder auth.RefusalRecorder
	Logger   *slog.Logger
	// HTTPClient, if set, is used to reach the IdP.
	HTTPClient *http.Client
	// Servers gives the Servers page its rows. Nil leaves the page out.
	Servers ServerSource
	// Pool lets the Blocks page write temporary blocks. A nil Pool leaves the
	// Blocks routes out.
	Pool *pgxpool.Pool
	// Rules gives the Blocks page the ToolgateRule objects. Nil shows none.
	Rules RuleSource
	// Secrets backs the Secrets page, and Auditor records its changes. Nil
	// Secrets leaves the page out.
	Secrets SecretStore
	Auditor secrets.Recorder
	// Grants backs the Grants pages. Nil leaves them out.
	Grants GrantSource
	// Workspaces backs the Workspaces page and the approval page of access
	// requests. Nil leaves them out.
	Workspaces WorkspaceSource
	// ApprovalTimeout is how long an approved call stays usable
	// (`approvalTimeout`). Zero means DefaultApprovalTimeout.
	ApprovalTimeout time.Duration
	// AuditPayloads reports whether audit payloads are on.
	AuditPayloads bool
	// Payloads reads payload objects from S3 for the audit event and
	// approval pages. Nil when payloads are off.
	Payloads PayloadSource
	// AuditOutput receives the audit lines of approval decisions. Nil
	// discards them; the rows are written either way.
	AuditOutput io.Writer
}

// UI is the web UI. Create it with New.
type UI struct {
	options   Options
	queries   *db.Queries
	registrar Registrar
	logger    *slog.Logger
	claims    claimNames

	limiter *auth.RateLimiter
	dropped prometheus.Counter

	mu       sync.Mutex
	routes   []Route
	provider *oidc.Provider
}

type claimNames struct {
	email, emailVerified, groups string
}

// New builds the UI and registers its sign-in routes, its pages and `GET /` on
// registrar. It does not contact the IdP: discovery runs at the first
// sign-in, so the gateway starts while the IdP is unreachable.
func New(options Options, registrar Registrar) *UI {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	u := &UI{
		options:   options,
		queries:   db.New(options.Database),
		registrar: registrar,
		logger:    logger,
		claims: claimNames{
			email:         valueOr(options.EmailClaim, "email"),
			emailVerified: valueOr(options.EmailVerifiedClaim, "email_verified"),
			groups:        valueOr(options.GroupsClaim, "groups"),
		},
		limiter: auth.NewRateLimiter(),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "toolgate_ui_auth_refusals_dropped_total",
			Help: "Failed web UI sign-ins not written to the audit log because their remote address passed 10 a minute.",
		}),
	}

	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}

	u.Handle("GET "+LoginPath, Public, http.HandlerFunc(u.login))
	u.Handle("GET "+CallbackPath, Public, http.HandlerFunc(u.callback))
	u.Handle("GET "+StaticPath, Public, staticFilesHandler(http.StripPrefix(StaticPath, http.FileServerFS(static))))
	u.Handle("POST "+LogoutPath, User, http.HandlerFunc(u.logout))
	u.Handle("GET /{$}", User, http.RedirectHandler(HomePath, http.StatusSeeOther))

	u.Handle("GET /ui/sessions", User, http.HandlerFunc(u.sessions))
	u.Handle("GET /ui/sessions/{id}", User, http.HandlerFunc(u.sessionPageHandler))
	u.Handle("GET /ui/audit", User, http.HandlerFunc(u.audit))
	u.Handle("GET /ui/audit/events/{key}", User, http.HandlerFunc(u.auditEventPage))
	u.Handle("GET /ui/audit/events/{key}/payload", User, http.HandlerFunc(u.auditEventPayload))

	if options.Servers != nil {
		u.Handle("GET "+HomePath, User, http.HandlerFunc(u.servers))
		u.Handle("POST "+RestartPath, User, http.HandlerFunc(u.restartInstance))
	}

	if options.Pool != nil {
		u.Handle("GET /ui/blocks", Admin, http.HandlerFunc(u.blocks))
		u.Handle("POST /ui/blocks", Admin, http.HandlerFunc(u.createBlock))
	}

	if options.Secrets != nil {
		u.Handle("GET /ui/secrets", User, http.HandlerFunc(u.secretsPage))
		u.handle("POST /ui/secrets", User, MaxSecretFormBytes, http.HandlerFunc(u.setSecret))
		u.Handle("POST /ui/secrets/{name}/delete", User, http.HandlerFunc(u.deleteSecret))
	}

	if options.Grants != nil {
		u.registerGrants()
	}

	if options.Workspaces != nil {
		u.registerWorkspaces()
	}

	u.registerApprovals()

	return u
}

//go:embed static
var staticFiles embed.FS

// DroppedRefusals is the metric counting failed sign-ins over the audit rate
// limit. Register it with the gateway's metrics registry.
func (u *UI) DroppedRefusals() prometheus.Counter {
	return u.dropped
}

// Routes returns the UI's routes and their access levels.
func (u *UI) Routes() []Route {
	u.mu.Lock()
	defer u.mu.Unlock()

	return slices.Clone(u.routes)
}

// Handle registers a UI route at an access level. Every UI handler
// registers through it. A User or Admin handler runs only with a valid UI
// session, which it finds with SessionFrom. Without one, a GET or HEAD is
// redirected to sign-in and anything else gets 401. An Admin handler gives
// 403 to a session outside the admin group.
func (u *UI) Handle(pattern string, access Access, handler http.Handler) {
	u.route(pattern, access, MaxFormBytes, nil, handler)
}

// handle is Handle with a body limit of limit bytes.
func (u *UI) handle(pattern string, access Access, limit int64, handler http.Handler) {
	u.route(pattern, access, limit, nil, handler)
}

// route registers a route with a body limit of limit bytes. A GET without a
// session is sent to signIn(request), or to a normal sign-in that returns to
// the page when signIn is nil.
func (u *UI) route(pattern string, access Access, limit int64, signIn func(*http.Request) string, handler http.Handler) {
	u.mu.Lock()
	u.routes = append(u.routes, Route{Pattern: pattern, Access: access})
	u.mu.Unlock()

	switch access {
	case Public:
		u.registrar.HandlePublic(pattern, protect(handler, limit))
	case User, Admin:
		u.registrar.Handle(pattern, protect(u.requireSession(access, signIn, requireCSRFToken(handler)), limit))
	default:
		panic(fmt.Sprintf("ui: route %s has an unknown access level %d", pattern, access))
	}
}

// Session is the UI session of a request.
type Session struct {
	// Principal is the ID token's subject.
	Principal string
	// Email is set only when the IdP verified it.
	Email  string
	Name   string
	Groups []string
	// CSRFToken is the session's synchronizer token.
	CSRFToken []byte
	// AuthTime is when the user last signed in at the IdP, or zero when the
	// IdP did not say.
	AuthTime time.Time
	// Admin reports membership of the admin group.
	Admin bool
}

type sessionKey struct{}

// SessionFrom returns the UI session that a User or Admin route loaded.
func SessionFrom(ctx context.Context) (Session, bool) {
	session, ok := ctx.Value(sessionKey{}).(Session)

	return session, ok
}

func (u *UI) requireSession(access Access, signIn func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		session, err := u.loadSession(request)
		if errors.Is(err, errNoSession) {
			if request.Method == http.MethodGet || request.Method == http.MethodHead {
				target := LoginPath + "?" + url.Values{"return_to": {request.URL.RequestURI()}}.Encode()
				if signIn != nil {
					target = signIn(request)
				}

				http.Redirect(writer, request, target, http.StatusSeeOther)

				return
			}

			http.Error(writer, "Your web UI session ended. Sign in again.", http.StatusUnauthorized)

			return
		}

		if err != nil {
			u.logger.ErrorContext(request.Context(), "cannot load the web UI session", "error", err)
			http.Error(writer, "The gateway can't load your session. Try again later.", http.StatusInternalServerError)

			return
		}

		if access == Admin && !session.Admin {
			http.Error(writer, "Only admins can open this page.", http.StatusForbidden)

			return
		}

		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), sessionKey{}, session)))
	})
}

var errNoSession = errors.New("no valid UI session")

// loadSession reads the session cookie and returns its valid session, moving
// last_seen_at forward at most once a minute.
func (u *UI) loadSession(request *http.Request) (Session, error) {
	cookie, err := request.Cookie(SessionCookie)
	if err != nil || cookie.Value == "" {
		return Session{}, errNoSession
	}

	idHash := hashValue(cookie.Value)

	row, err := u.queries.GetValidUISession(request.Context(), idHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, errNoSession
	}

	if err != nil {
		return Session{}, fmt.Errorf("load UI session: %w", err)
	}

	// The query moves last_seen_at only when it is a minute old, so this
	// skips the round trip in the common case.
	if !row.LastSeenAt.Valid || time.Since(row.LastSeenAt.Time) >= touchInterval {
		if err := u.queries.TouchUISession(request.Context(), idHash); err != nil {
			return Session{}, fmt.Errorf("touch UI session: %w", err)
		}
	}

	session := Session{
		Principal: row.Principal,
		Email:     row.Email,
		Name:      row.Name,
		Groups:    row.Groups,
		CSRFToken: row.CsrfToken,
		Admin:     u.options.AdminGroup != "" && slices.Contains(row.Groups, u.options.AdminGroup),
	}

	if row.AuthTime.Valid {
		session.AuthTime = row.AuthTime.Time
	}

	return session, nil
}

// oidcProvider returns the IdP's provider, discovering it on first use.
func (u *UI) oidcProvider(ctx context.Context) (*oidc.Provider, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.provider != nil {
		return u.provider, nil
	}

	ctx, cancel := context.WithTimeout(u.clientContext(ctx), discoveryTimeout)
	defer cancel()

	provider, err := oidc.NewProvider(ctx, u.options.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discover the IdP %s: %w", u.options.IssuerURL, err)
	}

	u.provider = provider

	return provider, nil
}

// clientContext carries the configured HTTP client for go-oidc and oauth2.
func (u *UI) clientContext(ctx context.Context) context.Context {
	if u.options.HTTPClient != nil {
		return oidc.ClientContext(ctx, u.options.HTTPClient)
	}

	return ctx
}

func (u *UI) oauthConfig(provider *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     u.options.ClientID,
		ClientSecret: u.options.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  strings.TrimSuffix(u.options.PublicURL, "/") + CallbackPath,
		Scopes:       u.options.Scopes,
	}
}

// randomValue returns 32 bytes from crypto/rand, base64url-encoded.
func randomValue() string {
	value := make([]byte, randomBytes)
	_, _ = rand.Read(value) // crypto/rand.Read never fails.

	return base64.RawURLEncoding.EncodeToString(value)
}

func hashValue(value string) []byte {
	sum := sha256.Sum256([]byte(value))

	return sum[:]
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
