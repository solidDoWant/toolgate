// Package tokenexchange is the test token endpoint: an HTTP handler that
// answers an RFC 8693 token exchange per subject. The token it issues holds
// the subject's `sub` and a counter for that subject. The test sets the
// lifetime and the subjects it refuses. The e2e tests serve it in the test
// process; the command in cmd/tokenexchange serves it on its own.
package tokenexchange

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	grantType        = "urn:ietf:params:oauth:grant-type:token-exchange"
	subjectTokenType = "urn:ietf:params:oauth:token-type:access_token"
)

// Request is an exchange the endpoint answered, as the gateway sent it.
type Request struct {
	// Subject is the `sub` of the subject token.
	Subject string
	// Form holds the request's form fields.
	Form url.Values
	// ClientID and ClientSecret are the Basic credentials of the request.
	ClientID, ClientSecret string
	// Issued is the token the endpoint returned, empty for a refusal.
	Issued string
}

// Endpoint issues "<prefix>-<sub>-<n>" to the n-th exchange of each subject.
type Endpoint struct {
	mu       sync.Mutex
	lifetime time.Duration
	counters map[string]int
	refused  map[string]string
	requests []Request
}

// New returns an endpoint whose tokens expire lifetime after the exchange.
func New(lifetime time.Duration) *Endpoint {
	return &Endpoint{lifetime: lifetime, counters: map[string]int{}, refused: map[string]string{}}
}

// SetLifetime changes how long the next tokens are valid.
func (e *Endpoint) SetLifetime(lifetime time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.lifetime = lifetime
}

// Refuse makes the endpoint answer subject with the OAuth error code.
func (e *Endpoint) Refuse(subject, code string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.refused[subject] = code
}

// Allow stops refusing subject.
func (e *Endpoint) Allow(subject string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.refused, subject)
}

// Requests returns the exchanges answered so far.
func (e *Endpoint) Requests() []Request {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]Request{}, e.requests...)
}

// Token is the token the endpoint issues to subject's n-th exchange.
func Token(subject string, n int) string {
	return fmt.Sprintf("exchanged-%s-%d", subject, n)
}

// ServeHTTP answers a token exchange POST. A request that is not a token
// exchange with a client secret and a subject token gets 400 or 401.
func (e *Endpoint) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	clientID, clientSecret, hasBasic := request.BasicAuth()
	if !hasBasic || clientSecret == "" {
		oauthError(writer, http.StatusUnauthorized, "invalid_client")

		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)

	if request.Method != http.MethodPost || request.ParseForm() != nil ||
		request.PostForm.Get("grant_type") != grantType ||
		request.PostForm.Get("subject_token_type") != subjectTokenType {
		oauthError(writer, http.StatusBadRequest, "invalid_request")

		return
	}

	subject, ok := subjectOf(request.PostForm.Get("subject_token"))
	if !ok {
		oauthError(writer, http.StatusBadRequest, "invalid_request")

		return
	}

	e.mu.Lock()
	record := Request{Subject: subject, Form: request.PostForm, ClientID: clientID, ClientSecret: clientSecret}

	if code, refused := e.refused[subject]; refused {
		e.requests = append(e.requests, record)
		e.mu.Unlock()
		oauthError(writer, http.StatusBadRequest, code)

		return
	}

	e.counters[subject]++
	record.Issued = Token(subject, e.counters[subject])
	e.requests = append(e.requests, record)
	lifetime := e.lifetime
	e.mu.Unlock()

	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"access_token":      record.Issued,
		"issued_token_type": subjectTokenType,
		"token_type":        "Bearer",
		"expires_in":        int(lifetime.Seconds()),
	})
}

// subjectOf reads the `sub` of a JWT without verifying it: the endpoint is a
// test double, and the gateway passes the token through.
func subjectOf(token string) (string, bool) {
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{
		jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512, jose.PS256, jose.EdDSA,
	})
	if err != nil {
		return "", false
	}

	var claims jwt.Claims
	if parsed.UnsafeClaimsWithoutVerification(&claims) != nil || claims.Subject == "" {
		return "", false
	}

	return claims.Subject, true
}

func oauthError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": code, "error_description": "refused by the test endpoint"})
}
