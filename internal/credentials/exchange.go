package credentials

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/solidDoWant/toolgate/internal/auth"
)

// Token exchange request values (RFC 8693).
const (
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccessToken   = "urn:ietf:params:oauth:token-type:access_token"
)

// MaxExchangedTokenSize is the largest access token an exchange accepts, so
// the token fits a delivery notification.
const MaxExchangedTokenSize = 4 << 10

// MinExchangedTokenLifetime is the shortest time an exchanged token must
// stay valid after the answer arrives.
const MinExchangedTokenLifetime = 2 * time.Minute

// maxErrorDescriptionLog is the most bytes of an error_description logged.
const maxErrorDescriptionLog = 256

// Kinds of exchange (the `kind` label and audit detail).
const (
	KindStart   = "start"
	KindRefresh = "refresh"
)

// Audit error codes of a failed exchange that isn't an OAuth error answer.
const (
	ErrorHTTPStatus   = "http-status"
	ErrorTimeout      = "timeout"
	ErrorUnreachable  = "unreachable"
	ErrorBadAnswer    = "bad-answer"
	ErrorClientSecret = "client-secret"
	// ErrorUnavailable is an exchange whose TokenExchange can't be used:
	// it is missing, isn't valid, or names a token endpoint that isn't
	// allowed. Nothing was sent.
	ErrorUnavailable = "unavailable"
)

// oauthErrorCode is the form of an OAuth error code the reason may name.
var oauthErrorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// jwtAlgorithms are the algorithms a token may name for its exp claim to be
// read. The signature is never checked, so the list only bounds parsing.
var jwtAlgorithms = []jose.SignatureAlgorithm{
	jose.EdDSA, jose.HS256, jose.HS384, jose.HS512,
	jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
}

// Exchanged is the result of one successful exchange.
type Exchanged struct {
	Token     Token
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// ExchangeError is a failed exchange. Its text names the exchange and a
// cause, and never a token, a client secret or the answer's body.
type ExchangeError struct {
	// Exchange is the exchange's name.
	Exchange string
	// Code is the audit error code: the OAuth error code, or one of the
	// Error* constants.
	Code string
	// Refused is true for an OAuth error answer.
	Refused bool

	reason string
}

// UnavailableError is the failure of an exchange whose TokenExchange can't
// be used. cause says why, and never holds a secret.
func UnavailableError(name string, cause error) *ExchangeError {
	return &ExchangeError{
		Exchange: name,
		Code:     ErrorUnavailable,
		reason:   fmt.Sprintf("Token exchange `%s` failed: %s", name, cause),
	}
}

// Error returns the reason text.
func (e *ExchangeError) Error() string { return e.reason }

// ExchangeSpec is one token exchange, as its TokenExchange resource
// describes it when the exchange runs.
type ExchangeSpec struct {
	// Name is the name templates use in `${exchange:NAME}`.
	Name               string
	TokenURL           string
	ClientID           string
	Audience           string
	Scope              string
	RequestedTokenType string
	// Timeout bounds one exchange.
	Timeout time.Duration
	// ClientSecret reads the client secret for one exchange, so a rotated
	// secret applies from the next exchange. Its error names where the
	// secret is, never a value.
	ClientSecret func(ctx context.Context) ([]byte, error)
}

// Exchanger runs token exchanges for one exchange spec.
type Exchanger struct {
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	spec    ExchangeSpec
	client  *http.Client
	metrics *Metrics
	logger  *slog.Logger
}

// NewTransport returns the transport exchanges use. caFile is a PEM bundle
// of the CAs the token endpoints are trusted with; empty means the system
// roots.
func NewTransport(caFile string) (*http.Transport, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("token exchange caFile: %w", err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("token exchange caFile %s has no PEM certificate", caFile)
		}

		tlsConfig.RootCAs = pool
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig

	return transport, nil
}

// NewExchanger returns the exchanger of spec, sending its requests through
// transport. Metrics and logger may be nil.
func NewExchanger(spec ExchangeSpec, transport http.RoundTripper, metrics *Metrics, logger *slog.Logger) *Exchanger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Exchanger{
		spec: spec,
		client: &http.Client{
			Transport: transport,
			Timeout:   spec.Timeout,
			// Redirects aren't followed: the request carries the subject
			// token and the client secret.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		metrics: metrics,
		logger:  logger,
	}
}

// Name is the name of the exchange.
func (e *Exchanger) Name() string { return e.spec.Name }

// Timeout bounds one exchange.
func (e *Exchanger) Timeout() time.Duration { return e.spec.Timeout }

func (e *Exchanger) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}

	return time.Now()
}

// Exchange sends one token exchange request for subject and checks the
// answer. kind is KindStart or KindRefresh. A failure is an *ExchangeError.
func (e *Exchanger) Exchange(ctx context.Context, subject auth.SubjectToken, kind string) (Exchanged, error) {
	exchanged, err := e.exchange(ctx, subject)

	outcome := "success"

	var exchangeErr *ExchangeError
	if errors.As(err, &exchangeErr) {
		outcome = "error"
		if exchangeErr.Refused {
			outcome = "refused"
		}
	}

	e.metrics.observe(e.spec.Name, kind, outcome)

	return exchanged, err
}

func (e *Exchanger) exchange(ctx context.Context, subject auth.SubjectToken) (Exchanged, error) {
	secret, err := e.clientSecret(ctx)
	if err != nil {
		return Exchanged{}, e.fail(ErrorClientSecret, false, "its client secret can't be read: "+err.Error())
	}

	params := url.Values{
		"grant_type":         {grantTypeTokenExchange},
		"subject_token":      {subject.Reveal()},
		"subject_token_type": {tokenTypeAccessToken},
	}
	if e.spec.Audience != "" {
		params.Set("audience", e.spec.Audience)
	}

	if e.spec.RequestedTokenType != "" {
		params.Set("requested_token_type", e.spec.RequestedTokenType)
	}

	// A new Config for each exchange, so nothing is cached between them.
	// The auth style is fixed, so auto-detection never sends the subject
	// token twice.
	request := &clientcredentials.Config{
		ClientID:       e.spec.ClientID,
		ClientSecret:   secret,
		TokenURL:       e.spec.TokenURL,
		Scopes:         strings.Fields(e.spec.Scope),
		AuthStyle:      oauth2.AuthStyleInHeader,
		EndpointParams: params,
	}

	answer, err := request.Token(context.WithValue(ctx, oauth2.HTTPClient, e.client))
	if err != nil {
		return Exchanged{}, e.requestError(err)
	}

	issuedAt := e.now()

	return e.checkAnswer(answer, issuedAt)
}

// clientSecret reads the client secret, without its trailing white space.
// A missing or empty secret is an error.
func (e *Exchanger) clientSecret(ctx context.Context) (string, error) {
	if e.spec.ClientSecret == nil {
		return "", errors.New("no client secret is set")
	}

	content, err := e.spec.ClientSecret(ctx)
	if err != nil {
		return "", err
	}

	secret := strings.TrimRightFunc(string(content), unicode.IsSpace)
	if secret == "" {
		return "", errors.New("the client secret is empty")
	}

	return secret, nil
}

// requestError maps a failed token request to its reason. It never uses the
// error's text: a RetrieveError holds the answer's body.
func (e *Exchanger) requestError(err error) *ExchangeError {
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		if retrieveErr.ErrorDescription != "" {
			e.logger.Warn("token endpoint error description",
				"exchange", e.spec.Name,
				"description", cleanDescription(retrieveErr.ErrorDescription))
		}

		if oauthErrorCode.MatchString(retrieveErr.ErrorCode) {
			return e.fail(retrieveErr.ErrorCode, true, fmt.Sprintf("the token endpoint answered `%s`.", retrieveErr.ErrorCode))
		}

		status := 0
		if retrieveErr.Response != nil {
			status = retrieveErr.Response.StatusCode
		}

		return e.fail(ErrorHTTPStatus, false, fmt.Sprintf("the token endpoint answered HTTP %d.", status))
	}

	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return e.fail(ErrorTimeout, false, fmt.Sprintf("the token endpoint didn't answer within %s.", e.spec.Timeout))
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return e.fail(ErrorUnreachable, false, "the token endpoint couldn't be reached.")
	}

	// The library's other errors are a 2xx answer it can't read or one
	// without an access token.
	return e.fail(ErrorBadAnswer, false, "the answer has no access token")
}

// checkAnswer checks a token answer and returns the exchanged token.
func (e *Exchanger) checkAnswer(answer *oauth2.Token, issuedAt time.Time) (Exchanged, error) {
	if answer.AccessToken == "" {
		return Exchanged{}, e.fail(ErrorBadAnswer, false, "the answer has no access token")
	}

	if len(answer.AccessToken) > MaxExchangedTokenSize {
		return Exchanged{}, e.fail(ErrorBadAnswer, false, "the answer has a token larger than 4 KiB")
	}

	if e.spec.RequestedTokenType != "" {
		issued, _ := answer.Extra("issued_token_type").(string)
		if issued != e.spec.RequestedTokenType {
			return Exchanged{}, e.fail(ErrorBadAnswer, false, "the answer has a different `issued_token_type`")
		}
	}

	expiresAt, ok := answerExpiry(answer, issuedAt)
	if !ok {
		return Exchanged{}, e.fail(ErrorBadAnswer, false, "the answer has no expiry")
	}

	if expiresAt.Sub(issuedAt) < MinExchangedTokenLifetime {
		return Exchanged{}, e.fail(ErrorBadAnswer, false, "the answer has an expiry less than 2 minutes away")
	}

	return Exchanged{Token: Token(answer.AccessToken), IssuedAt: issuedAt, ExpiresAt: expiresAt}, nil
}

// answerExpiry is the earlier of the expires_in time and the token's exp
// claim, when the token is a JWT. The claim is read without verification:
// toolgate passes the token through and only plans with exp.
func answerExpiry(answer *oauth2.Token, issuedAt time.Time) (time.Time, bool) {
	var expiry time.Time

	if seconds, ok := expiresIn(answer); ok {
		expiry = issuedAt.Add(time.Duration(seconds) * time.Second)
	} else if !answer.Expiry.IsZero() {
		expiry = answer.Expiry
	}

	if parsed, err := jwt.ParseSigned(answer.AccessToken, jwtAlgorithms); err == nil {
		var claims jwt.Claims
		if err := parsed.UnsafeClaimsWithoutVerification(&claims); err == nil && claims.Expiry != nil {
			if exp := claims.Expiry.Time(); expiry.IsZero() || exp.Before(expiry) {
				expiry = exp
			}
		}
	}

	return expiry, !expiry.IsZero()
}

// expiresIn reads expires_in from the answer, measured from the gateway's
// clock rather than the library's.
func expiresIn(answer *oauth2.Token) (int64, bool) {
	switch value := answer.Extra("expires_in").(type) {
	case float64:
		return int64(value), value > 0
	case string:
		seconds, err := strconv.ParseInt(value, 10, 64)

		return seconds, err == nil && seconds > 0
	}

	return 0, false
}

// cleanDescription removes control characters and cuts the text to
// maxErrorDescriptionLog bytes, on a character boundary.
func cleanDescription(description string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}

		return r
	}, description)

	if len(cleaned) <= maxErrorDescriptionLog {
		return cleaned
	}

	cut := maxErrorDescriptionLog
	for cut > 0 && !utf8.RuneStart(cleaned[cut]) {
		cut--
	}

	return cleaned[:cut]
}

func (e *Exchanger) fail(code string, refused bool, cause string) *ExchangeError {
	return &ExchangeError{
		Exchange: e.spec.Name,
		Code:     code,
		Refused:  refused,
		reason:   fmt.Sprintf("Token exchange `%s` failed: %s", e.spec.Name, cause),
	}
}
