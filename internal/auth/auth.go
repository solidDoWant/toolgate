// Package auth authenticates requests to the gateway and builds their
// identity (SPEC.md §5–§6). Users sign in with an access token from the IdP,
// which is checked against the IdP's JWKS, the configured audience and its
// expiry. The X-Toolgate-Session and X-Toolgate-Workspace headers are checked
// after the token.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/prometheus/client_golang/prometheus"
)

// Header names and limits.
const (
	SessionHeader   = "X-Toolgate-Session"
	WorkspaceHeader = "X-Toolgate-Workspace"

	// MaxAuthorizationBytes bounds the Authorization header.
	MaxAuthorizationBytes = 16 << 10

	// DefaultWorkspace is the workspace of a user request without a
	// workspace header, before the claimed/ prefix.
	DefaultWorkspace = "default"

	// claimedPrefix marks a workspace the client claimed and nobody verified.
	claimedPrefix = "claimed/"

	// maxRefusalField bounds the unverified issuer and audience values copied
	// from a refused token into its audit event.
	maxRefusalField = 256
	maxRefusalList  = 8
)

// Attestation levels (SPEC.md §6).
const (
	AttestationUser     = "user"
	AttestationAttested = "attested"
)

// headerValuePattern is the pattern both identity headers must match.
var headerValuePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Identity is the verified identity of a request plus what the client claimed
// about its environment.
type Identity struct {
	// Principal is the OIDC subject of a user.
	Principal string `json:"principal"`
	// Email is set only when the token's email-verified claim is true.
	Email string `json:"email,omitempty"`
	// Groups come from the configured groups claim.
	Groups []string `json:"groups,omitempty"`
	// Workspace is `claimed/<header>`, or `claimed/default` without a header.
	Workspace string `json:"workspace"`
	// Session is the X-Toolgate-Session header. Nobody verifies it.
	Session string `json:"session,omitempty"`
	// Attestation is AttestationUser for users.
	Attestation string `json:"attestation"`
}

type identityKey struct{}

// IdentityFrom returns the identity Middleware stored in ctx.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityKey{}).(Identity)

	return identity, ok
}

// Refusal describes a failed sign-in for the `auth.refuse` audit event. It
// never holds the token or a principal.
type Refusal struct {
	// Reason is a short, fixed description of why the request was refused.
	Reason string
	// Issuer and Audience are the token's own claims, read without
	// verification, so they show what the client sent. They are empty when
	// the token can't be parsed.
	Issuer   string
	Audience []string
	// RemoteAddress is the client's IP address.
	RemoteAddress string
}

// RefusalRecorder writes refusals to the audit log.
type RefusalRecorder interface {
	RecordAuthRefusal(ctx context.Context, refusal Refusal)
}

// Options configures an Authenticator.
type Options struct {
	// IssuerURL is the IdP's issuer. Its discovery document names the JWKS.
	IssuerURL string
	// Audience must be one of the token's audiences.
	Audience string
	// Claim names. Empty names take the defaults `email`, `email_verified`
	// and `groups`.
	EmailClaim         string
	EmailVerifiedClaim string
	GroupsClaim        string
	// Recorder receives failed sign-ins. Nil records nothing.
	Recorder RefusalRecorder
}

// Authenticator checks user access tokens and the identity headers.
type Authenticator struct {
	verifier *oidc.IDTokenVerifier
	audience string
	claims   claimNames
	recorder RefusalRecorder
	limiter  *refusalLimiter
	dropped  prometheus.Counter
}

type claimNames struct {
	email, emailVerified, groups string
}

// New builds an Authenticator. It fetches the IdP's discovery document, so
// the IdP must be reachable. Signing keys are fetched when first needed and
// cached; an unknown key ID fetches them again.
func New(ctx context.Context, options Options) (*Authenticator, error) {
	if options.IssuerURL == "" {
		return nil, errors.New("auth: the issuer URL is required")
	}

	if options.Audience == "" {
		return nil, errors.New("auth: the audience is required")
	}

	provider, err := oidc.NewProvider(ctx, options.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("auth: discovering the IdP %s: %w", options.IssuerURL, err)
	}

	claims := claimNames{
		email:         valueOr(options.EmailClaim, "email"),
		emailVerified: valueOr(options.EmailVerifiedClaim, "email_verified"),
		groups:        valueOr(options.GroupsClaim, "groups"),
	}

	return &Authenticator{
		verifier: provider.VerifierContext(context.WithoutCancel(ctx), &oidc.Config{ClientID: options.Audience}),
		audience: options.Audience,
		claims:   claims,
		recorder: options.Recorder,
		limiter:  newRefusalLimiter(),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "toolgate_auth_refusals_dropped_total",
			Help: "Failed sign-ins not written to the audit log because their remote address passed 10 a minute.",
		}),
	}, nil
}

// DroppedRefusals is the metric counting failed sign-ins over the audit rate
// limit. Register it with the gateway's metrics registry.
func (a *Authenticator) DroppedRefusals() prometheus.Counter {
	return a.dropped
}

// Middleware authenticates every request before next runs. A request without
// a valid token gets 401. Then the identity headers are checked: a value not
// matching `^[A-Za-z0-9._-]{1,128}$` gets 400, and so does a missing session
// header when requireSession is set (MCP requests). next finds the identity
// with IdentityFrom.
func (a *Authenticator) Middleware(next http.Handler, requireSession bool) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identity, refusal := a.authenticate(request)
		if refusal != nil {
			a.refuse(request, *refusal)
			writer.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

			return
		}

		session, sessionSet, sessionValid := headerValue(request, SessionHeader)
		if !sessionValid || (requireSession && !sessionSet) {
			http.Error(writer, "The X-Toolgate-Session header is missing or invalid. It must match ^[A-Za-z0-9._-]{1,128}$.", http.StatusBadRequest)

			return
		}

		workspace, workspaceSet, workspaceValid := headerValue(request, WorkspaceHeader)
		if !workspaceValid {
			http.Error(writer, "The X-Toolgate-Workspace header is invalid. It must match ^[A-Za-z0-9._-]{1,128}$.", http.StatusBadRequest)

			return
		}

		if !workspaceSet {
			workspace = DefaultWorkspace
		}

		identity.Session = session
		identity.Workspace = claimedPrefix + workspace

		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), identityKey{}, identity)))
	})
}

// headerValue returns the header's value, whether it was sent with a value,
// and whether that value is valid. An absent or empty header is valid and not
// set. A header sent more than once is invalid.
func headerValue(request *http.Request, name string) (value string, set, valid bool) {
	values := request.Header.Values(name)

	switch {
	case len(values) == 0 || (len(values) == 1 && values[0] == ""):
		return "", false, true
	case len(values) > 1:
		return "", true, false
	}

	return values[0], true, headerValuePattern.MatchString(values[0])
}

// authenticate verifies the bearer token. It returns the identity without the
// header fields, or the refusal to record.
func (a *Authenticator) authenticate(request *http.Request) (Identity, *Refusal) {
	header := request.Header.Values("Authorization")
	if len(header) != 1 {
		return Identity{}, &Refusal{Reason: "missing token"}
	}

	if len(header[0]) > MaxAuthorizationBytes {
		return Identity{}, &Refusal{Reason: "token too large"}
	}

	scheme, token, found := strings.Cut(header[0], " ")
	token = strings.TrimSpace(token)

	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return Identity{}, &Refusal{Reason: "missing token"}
	}

	verified, err := a.verifier.Verify(request.Context(), token)
	if err != nil {
		refusal := unverifiedRefusal(token)
		refusal.Reason = "invalid token"

		return Identity{}, &refusal
	}

	var claims map[string]json.RawMessage
	if err := verified.Claims(&claims); err != nil {
		return Identity{}, &Refusal{Reason: "invalid claims", Issuer: verified.Issuer, Audience: verified.Audience}
	}

	identity, err := a.identityFromClaims(verified.Subject, claims)
	if err != nil {
		return Identity{}, &Refusal{Reason: "invalid claims", Issuer: verified.Issuer, Audience: verified.Audience}
	}

	return identity, nil
}

func (a *Authenticator) identityFromClaims(subject string, claims map[string]json.RawMessage) (Identity, error) {
	if subject == "" {
		return Identity{}, errors.New("no subject")
	}

	identity := Identity{Principal: subject, Attestation: AttestationUser}

	// An unverified email is treated as absent (docs/design/phase-1.md §7).
	var verifiedEmail bool
	if raw, ok := claims[a.claims.emailVerified]; ok {
		_ = json.Unmarshal(raw, &verifiedEmail)
	}

	if raw, ok := claims[a.claims.email]; ok && verifiedEmail {
		if err := json.Unmarshal(raw, &identity.Email); err != nil {
			return Identity{}, fmt.Errorf("email claim: %w", err)
		}
	}

	// A groups claim that isn't a list of strings is refused rather than
	// ignored, because dropping groups could skip a rule that denies one.
	if raw, ok := claims[a.claims.groups]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &identity.Groups); err != nil {
			return Identity{}, fmt.Errorf("groups claim: %w", err)
		}
	}

	return identity, nil
}

// unverifiedRefusal reads the issuer and audience of a token that failed
// verification, without checking its signature, for the audit event.
func unverifiedRefusal(token string) Refusal {
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{
		jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512,
		jose.PS256, jose.PS384, jose.PS512, jose.EdDSA,
	})
	if err != nil {
		return Refusal{}
	}

	var claims jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return Refusal{}
	}

	audience := []string(claims.Audience)
	if len(audience) > maxRefusalList {
		audience = audience[:maxRefusalList]
	}

	bounded := make([]string, 0, len(audience))
	for _, value := range audience {
		bounded = append(bounded, truncate(value))
	}

	return Refusal{Issuer: truncate(claims.Issuer), Audience: bounded}
}

func (a *Authenticator) refuse(request *http.Request, refusal Refusal) {
	refusal.RemoteAddress = remoteIP(request)

	if !a.limiter.allow(refusal.RemoteAddress) {
		a.dropped.Inc()

		return
	}

	if a.recorder != nil {
		a.recorder.RecordAuthRefusal(request.Context(), refusal)
	}
}

func remoteIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}

	return host
}

func truncate(value string) string {
	if len(value) > maxRefusalField {
		return value[:maxRefusalField]
	}

	return value
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
