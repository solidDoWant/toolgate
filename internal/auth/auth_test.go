package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	tgtestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

// recorder keeps the refusals the authenticator records.
type recorder struct {
	mutex    sync.Mutex
	refusals []auth.Refusal
}

func (r *recorder) RecordAuthRefusal(_ context.Context, refusal auth.Refusal) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.refusals = append(r.refusals, refusal)
}

func (r *recorder) all() []auth.Refusal {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return append([]auth.Refusal(nil), r.refusals...)
}

type fixture struct {
	provider      *tgtestutil.OIDCProvider
	recorder      *recorder
	authenticator *auth.Authenticator
	handler       http.Handler
}

// newFixture starts a fake IdP and an authenticator for it. The wrapped
// handler answers 200 with the request's identity as JSON.
func newFixture(t *testing.T, options auth.Options, requireSession bool) fixture {
	t.Helper()

	provider := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{})
	rec := &recorder{}

	options.IssuerURL = provider.Issuer
	if options.Audiences == nil {
		options.Audiences = []string{provider.Audience}
	}

	options.Recorder = rec

	authenticator, err := auth.New(t.Context(), options)
	require.NoError(t, err)

	next := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identity, ok := auth.IdentityFrom(request.Context())
		if !ok {
			http.Error(writer, "no identity", http.StatusInternalServerError)

			return
		}

		writer.Header().Set("X-Test-Expiry", strconv.FormatInt(identity.Expiry.Unix(), 10))
		_ = json.NewEncoder(writer).Encode(identity)
	})

	return fixture{
		provider:      provider,
		recorder:      rec,
		authenticator: authenticator,
		handler:       authenticator.Middleware(next, requireSession),
	}
}

func (f fixture) do(t *testing.T, token string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	request.RemoteAddr = "192.0.2.10:40000"

	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)

	return response
}

func TestAC1_03BadTokensAreRefused(t *testing.T) {
	t.Parallel()

	session := map[string]string{"X-Toolgate-Session": "session-1"}

	tests := []struct {
		name         string
		token        func(provider *tgtestutil.OIDCProvider) string
		authorize    string
		wantReason   string
		wantIssuer   string
		wantAudience bool
	}{
		{
			name:       "[AC-1-03] no token",
			token:      func(*tgtestutil.OIDCProvider) string { return "" },
			wantReason: "missing token",
		},
		{
			name: "[AC-1-03] expired token",
			token: func(provider *tgtestutil.OIDCProvider) string {
				return provider.MintAccessToken(t, tgtestutil.TokenClaims{
					Subject:   "alice",
					IssuedAt:  time.Now().Add(-2 * time.Hour),
					ExpiresAt: time.Now().Add(-time.Hour),
				})
			},
			wantReason:   "invalid token",
			wantAudience: true,
		},
		{
			name: "[AC-1-03] token for another audience",
			token: func(provider *tgtestutil.OIDCProvider) string {
				return provider.MintAccessToken(t, tgtestutil.TokenClaims{
					Subject:  "alice",
					Audience: []string{"another-service"},
				})
			},
			wantReason: "audience not allowed",
		},
		{
			name: "[AC-1-03] token from another issuer",
			token: func(provider *tgtestutil.OIDCProvider) string {
				return provider.MintAccessToken(t, tgtestutil.TokenClaims{
					Subject: "alice",
					Issuer:  "https://other-idp.example",
				})
			},
			wantReason:   "invalid token",
			wantIssuer:   "https://other-idp.example",
			wantAudience: true,
		},
		{
			name: "[AC-1-03] token signed by another provider",
			token: func(*tgtestutil.OIDCProvider) string {
				other := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{})

				return other.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice"})
			},
			wantReason:   "invalid token",
			wantAudience: true,
		},
		{
			name:       "[AC-1-03] not a bearer token",
			token:      func(*tgtestutil.OIDCProvider) string { return "" },
			authorize:  "Basic YWxpY2U6c2VjcmV0",
			wantReason: "missing token",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, auth.Options{}, true)

			token := test.token(f.provider)
			headers := map[string]string{"X-Toolgate-Session": session["X-Toolgate-Session"]}

			if test.authorize != "" {
				headers["Authorization"] = test.authorize
			}

			response := f.do(t, token, headers)

			assert.Equal(t, http.StatusUnauthorized, response.Code)
			assert.Equal(t, "Bearer", response.Header().Get("WWW-Authenticate"))
			assert.NotContains(t, response.Body.String(), "alice")

			refusals := f.recorder.all()
			require.Len(t, refusals, 1)
			assert.Equal(t, test.wantReason, refusals[0].Reason)
			assert.Equal(t, "192.0.2.10", refusals[0].RemoteAddress)

			if test.wantIssuer != "" {
				assert.Equal(t, test.wantIssuer, refusals[0].Issuer)
			}

			if test.wantAudience {
				assert.Equal(t, []string{f.provider.Audience}, refusals[0].Audience)
			}

			if token != "" {
				assert.NotContains(t, refusals[0].Reason, token)
			}
		})
	}
}

func TestAuthorizationHeaderOverLimitIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t, auth.Options{}, true)
	session := map[string]string{"X-Toolgate-Session": "session-1"}

	valid := f.provider.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice"})

	// "Bearer " plus the token, padded with spaces to exactly the limit, is
	// accepted; one more byte is refused.
	atLimit := "Bearer " + valid + strings.Repeat(" ", auth.MaxAuthorizationBytes-len("Bearer ")-len(valid))
	require.Len(t, atLimit, auth.MaxAuthorizationBytes)

	response := f.do(t, "", map[string]string{"Authorization": atLimit, "X-Toolgate-Session": "session-1"})
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())

	response = f.do(t, "", map[string]string{"Authorization": atLimit + " ", "X-Toolgate-Session": session["X-Toolgate-Session"]})
	assert.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestValidTokenBuildsIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options auth.Options
		claims  tgtestutil.TokenClaims
		headers map[string]string
		want    auth.Identity
	}{
		{
			name: "default claims and claimed workspace",
			claims: tgtestutil.TokenClaims{
				Subject:       "alice",
				Email:         "alice@example.com",
				EmailVerified: true,
				Groups:        []string{"dev", "ops"},
			},
			headers: map[string]string{"X-Toolgate-Session": "s-1", "X-Toolgate-Workspace": "repo.a_b-c"},
			want: auth.Identity{
				Principal:   "alice",
				Email:       "alice@example.com",
				Groups:      []string{"dev", "ops"},
				Workspace:   "claimed/repo.a_b-c",
				Session:     "s-1",
				Kind:        auth.KindUser,
				Attestation: auth.AttestationUser,
			},
		},
		{
			name:    "no workspace header means the default workspace",
			claims:  tgtestutil.TokenClaims{Subject: "bob"},
			headers: map[string]string{"X-Toolgate-Session": "s-2"},
			want: auth.Identity{
				Principal:   "bob",
				Workspace:   "claimed/default",
				Session:     "s-2",
				Kind:        auth.KindUser,
				Attestation: auth.AttestationUser,
			},
		},
		{
			name: "unverified email is dropped",
			claims: tgtestutil.TokenClaims{
				Subject:       "carol",
				Email:         "carol@example.com",
				EmailVerified: false,
			},
			headers: map[string]string{"X-Toolgate-Session": "s-3"},
			want: auth.Identity{
				Principal:   "carol",
				Workspace:   "claimed/default",
				Session:     "s-3",
				Kind:        auth.KindUser,
				Attestation: auth.AttestationUser,
			},
		},
		{
			name: "configured claim names",
			options: auth.Options{
				EmailClaim:         "mail",
				EmailVerifiedClaim: "mail_ok",
				GroupsClaim:        "roles",
			},
			claims: tgtestutil.TokenClaims{
				Subject:       "dave",
				Email:         "ignored@example.com",
				EmailVerified: true,
				Groups:        []string{"ignored"},
				Extra: map[string]any{
					"mail":    "dave@example.com",
					"mail_ok": true,
					"roles":   []string{"admins"},
				},
			},
			headers: map[string]string{"X-Toolgate-Session": "s-4"},
			want: auth.Identity{
				Principal:   "dave",
				Email:       "dave@example.com",
				Groups:      []string{"admins"},
				Workspace:   "claimed/default",
				Session:     "s-4",
				Kind:        auth.KindUser,
				Attestation: auth.AttestationUser,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, test.options, true)
			token := f.provider.MintAccessToken(t, test.claims)

			response := f.do(t, token, test.headers)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())

			var got auth.Identity
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
			assert.Equal(t, test.want, got)
			assert.Empty(t, f.recorder.all())
		})
	}
}

func TestIdentityCarriesTheTokenExpiry(t *testing.T) {
	t.Parallel()

	f := newFixture(t, auth.Options{}, true)
	expiry := time.Now().Add(17 * time.Minute).Truncate(time.Second)
	token := f.provider.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "erin", ExpiresAt: expiry})

	response := f.do(t, token, map[string]string{"X-Toolgate-Session": "s-5"})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, strconv.FormatInt(expiry.Unix(), 10), response.Header().Get("X-Test-Expiry"))
}

func TestMalformedGroupsClaimIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t, auth.Options{}, true)
	token := f.provider.MintAccessToken(t, tgtestutil.TokenClaims{
		Subject: "alice",
		Extra:   map[string]any{"groups": "not-a-list"},
	})

	response := f.do(t, token, map[string]string{"X-Toolgate-Session": "s-1"})
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	require.Len(t, f.recorder.all(), 1)
}

func TestAC1_10IdentityHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		requireSession bool
		headers        map[string]string
		wantStatus     int
	}{
		{
			name:           "[AC-1-10] session and workspace of 128 characters are accepted",
			requireSession: true,
			headers: map[string]string{
				"X-Toolgate-Session":   strings.Repeat("s", 128),
				"X-Toolgate-Workspace": strings.Repeat("w", 128),
			},
			wantStatus: http.StatusOK,
		},
		{
			name:           "[AC-1-10] session of 129 characters",
			requireSession: true,
			headers:        map[string]string{"X-Toolgate-Session": strings.Repeat("s", 129)},
			wantStatus:     http.StatusBadRequest,
		},
		{
			name:           "[AC-1-10] workspace of 129 characters",
			requireSession: true,
			headers: map[string]string{
				"X-Toolgate-Session":   "s-1",
				"X-Toolgate-Workspace": strings.Repeat("w", 129),
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:           "[AC-1-10] session with YAML characters",
			requireSession: true,
			headers:        map[string]string{"X-Toolgate-Session": "a: b"},
			wantStatus:     http.StatusBadRequest,
		},
		{
			name:           "[AC-1-10] workspace with a slash",
			requireSession: true,
			headers: map[string]string{
				"X-Toolgate-Session":   "s-1",
				"X-Toolgate-Workspace": "k8s/ci/Pod/x",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:           "[AC-1-10] workspace with a newline",
			requireSession: true,
			headers: map[string]string{
				"X-Toolgate-Session":   "s-1",
				"X-Toolgate-Workspace": "a\nb",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:           "[AC-1-10] empty workspace header",
			requireSession: true,
			headers: map[string]string{
				"X-Toolgate-Session":   "s-1",
				"X-Toolgate-Workspace": "",
			},
			wantStatus: http.StatusOK,
		},
		{
			name:           "[AC-1-10] missing session header on MCP requests",
			requireSession: true,
			headers:        map[string]string{},
			wantStatus:     http.StatusBadRequest,
		},
		{
			name:           "missing session header where it is not required",
			requireSession: false,
			headers:        map[string]string{},
			wantStatus:     http.StatusOK,
		},
		{
			name:           "[AC-1-10] invalid session header where it is not required",
			requireSession: false,
			headers:        map[string]string{"X-Toolgate-Session": "a b"},
			wantStatus:     http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, auth.Options{}, test.requireSession)
			token := f.provider.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice"})

			response := f.do(t, token, test.headers)
			assert.Equal(t, test.wantStatus, response.Code, response.Body.String())
		})
	}
}

func TestBadHeadersWithoutTokenGet401(t *testing.T) {
	t.Parallel()

	f := newFixture(t, auth.Options{}, true)

	response := f.do(t, "", map[string]string{"X-Toolgate-Session": "a: b"})
	assert.Equal(t, http.StatusUnauthorized, response.Code)
}

func TestRefusalsAreRecordedAtMostTenPerMinutePerAddress(t *testing.T) {
	t.Parallel()

	f := newFixture(t, auth.Options{}, true)

	for range 11 {
		response := f.do(t, "", nil)
		require.Equal(t, http.StatusUnauthorized, response.Code)
	}

	assert.Len(t, f.recorder.all(), 10)
	assert.InDelta(t, 1, testutil.ToFloat64(f.authenticator.DroppedRefusals()), 0)

	// Another address has its own budget.
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	request.RemoteAddr = "198.51.100.7:1234"
	f.handler.ServeHTTP(httptest.NewRecorder(), request)

	assert.Len(t, f.recorder.all(), 11)
}

func TestNewRequiresIssuerAndAudience(t *testing.T) {
	t.Parallel()

	_, err := auth.New(t.Context(), auth.Options{Audiences: []string{"toolgate"}})
	require.Error(t, err)

	_, err = auth.New(t.Context(), auth.Options{IssuerURL: "http://127.0.0.1:1"})
	require.Error(t, err)
}
