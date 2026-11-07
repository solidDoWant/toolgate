package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	tgtestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

func TestTokensFromEachConfiguredIssuerAreAccepted(t *testing.T) {
	t.Parallel()

	primary := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{Audience: "toolgate"})
	coder := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{Audience: "coder-toolgate"})
	unconfigured := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{Audience: "toolgate"})

	f := newIssuersFixture(t, auth.Options{
		IssuerURL: primary.Issuer,
		Audiences: []string{"toolgate"},
		AdditionalIssuers: []auth.IssuerOptions{
			{IssuerURL: coder.Issuer, Audiences: []string{"coder-toolgate"}},
		},
	})

	claims := tgtestutil.TokenClaims{
		Subject:       "alice",
		Email:         "alice@example.com",
		EmailVerified: true,
		Groups:        []string{"dev"},
	}

	tests := []struct {
		name       string
		token      func() string
		wantStatus int
		wantReason string
	}{
		{
			name:       "the primary issuer with its audience",
			token:      func() string { return primary.MintAccessToken(t, claims) },
			wantStatus: http.StatusOK,
		},
		{
			name:       "the additional issuer with its audience",
			token:      func() string { return coder.MintAccessToken(t, claims) },
			wantStatus: http.StatusOK,
		},
		{
			name:       "an issuer that is not configured",
			token:      func() string { return unconfigured.MintAccessToken(t, claims) },
			wantStatus: http.StatusUnauthorized,
			wantReason: "invalid token",
		},
		{
			name: "the additional issuer with the primary issuer's audience",
			token: func() string {
				wrong := claims
				wrong.Audience = []string{"toolgate"}

				return coder.MintAccessToken(t, wrong)
			},
			wantStatus: http.StatusUnauthorized,
			wantReason: "audience not allowed",
		},
		{
			name: "the primary issuer with the additional issuer's audience",
			token: func() string {
				wrong := claims
				wrong.Audience = []string{"coder-toolgate"}

				return primary.MintAccessToken(t, wrong)
			},
			wantStatus: http.StatusUnauthorized,
			wantReason: "audience not allowed",
		},
		{
			name: "an additional issuer's claim on a token another issuer signed",
			token: func() string {
				forged := claims
				forged.Issuer = coder.Issuer
				forged.Audience = []string{"coder-toolgate"}

				return primary.MintAccessToken(t, forged)
			},
			wantStatus: http.StatusUnauthorized,
			wantReason: "invalid token",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, refusals := f.do(t, test.token())
			require.Equal(t, test.wantStatus, response.Code, response.Body.String())

			if test.wantStatus != http.StatusOK {
				require.Len(t, refusals, 1)
				assert.Equal(t, test.wantReason, refusals[0].Reason)

				return
			}

			assert.Empty(t, refusals)

			var identity auth.Identity
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &identity))
			assert.Equal(t, auth.Identity{
				Principal:   "alice",
				Email:       "alice@example.com",
				Groups:      []string{"dev"},
				Workspace:   "claimed/default",
				Session:     "s-1",
				Kind:        auth.KindUser,
				Attestation: auth.AttestationUser,
			}, identity)
		})
	}
}

func TestAnUnreachableAdditionalIssuerRefusesOnlyItsOwnTokens(t *testing.T) {
	t.Parallel()

	primary := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{Audience: "toolgate"})
	coder := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{Audience: "coder-toolgate"})
	down := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{Audience: "down-toolgate"})
	downToken := down.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice"})
	down.Close()

	f := newIssuersFixture(t, auth.Options{
		IssuerURL: primary.Issuer,
		Audiences: []string{"toolgate"},
		AdditionalIssuers: []auth.IssuerOptions{
			{IssuerURL: down.Issuer, Audiences: []string{"down-toolgate"}},
			{IssuerURL: coder.Issuer, Audiences: []string{"coder-toolgate"}},
		},
	})

	response, refusals := f.do(t, downToken)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	require.Len(t, refusals, 1)
	assert.Equal(t, "issuer unavailable", refusals[0].Reason)

	response, _ = f.do(t, primary.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice"}))
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())

	response, _ = f.do(t, coder.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice"}))
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestNewRefusesAnInvalidAdditionalIssuer(t *testing.T) {
	t.Parallel()

	primary := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{Audience: "toolgate"})

	tests := []struct {
		name   string
		issuer auth.IssuerOptions
	}{
		{name: "no issuer URL", issuer: auth.IssuerOptions{Audiences: []string{"coder"}}},
		{name: "no audience", issuer: auth.IssuerOptions{IssuerURL: "https://idp.example/coder/"}},
		{name: "an empty audience", issuer: auth.IssuerOptions{IssuerURL: "https://idp.example/coder/", Audiences: []string{""}}},
		{name: "the primary issuer again", issuer: auth.IssuerOptions{IssuerURL: primary.Issuer, Audiences: []string{"coder"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := auth.New(t.Context(), auth.Options{
				IssuerURL:         primary.Issuer,
				Audiences:         []string{"toolgate"},
				AdditionalIssuers: []auth.IssuerOptions{test.issuer},
			})
			require.Error(t, err)
		})
	}
}

// issuersFixture is an authenticator whose wrapped handler answers 200 with
// the request's identity as JSON.
type issuersFixture struct {
	recorder *recorder
	handler  http.Handler
}

func newIssuersFixture(t *testing.T, options auth.Options) issuersFixture {
	t.Helper()

	rec := &recorder{}
	options.Recorder = rec

	authenticator, err := auth.New(t.Context(), options)
	require.NoError(t, err)

	next := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identity, _ := auth.IdentityFrom(request.Context())
		_ = json.NewEncoder(writer).Encode(identity)
	})

	return issuersFixture{recorder: rec, handler: authenticator.Middleware(next, false)}
}

// do sends one request with the token and returns the response and the
// refusals it recorded.
func (f issuersFixture) do(t *testing.T, token string) (*httptest.ResponseRecorder, []auth.Refusal) {
	t.Helper()

	before := len(f.recorder.all())

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api", nil)
	request.RemoteAddr = "192.0.2.10:40000"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(auth.SessionHeader, "s-1")

	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)

	return response, f.recorder.all()[before:]
}
