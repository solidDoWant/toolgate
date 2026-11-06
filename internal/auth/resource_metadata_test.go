package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	tgtestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

const metadataURL = "https://mcp.example.com/.well-known/oauth-protected-resource/mcp"

func TestMCPRefusalNamesTheResourceMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		token     func(provider *tgtestutil.OIDCProvider) string
		authorize string
		want      string
	}{
		{
			name:  "no token",
			token: func(*tgtestutil.OIDCProvider) string { return "" },
			want:  `Bearer resource_metadata="` + metadataURL + `"`,
		},
		{
			name:      "not a bearer token",
			token:     func(*tgtestutil.OIDCProvider) string { return "" },
			authorize: "Basic YWxpY2U6c2VjcmV0",
			want:      `Bearer resource_metadata="` + metadataURL + `"`,
		},
		{
			name:  "garbage token",
			token: func(*tgtestutil.OIDCProvider) string { return "not-a-jwt" },
			want:  `Bearer resource_metadata="` + metadataURL + `", error="invalid_token"`,
		},
		{
			name: "token for another audience",
			token: func(provider *tgtestutil.OIDCProvider) string {
				return provider.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice", Audience: []string{"another-service"}})
			},
			want: `Bearer resource_metadata="` + metadataURL + `", error="invalid_token"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, auth.Options{ResourceMetadataURL: metadataURL}, true)

			headers := map[string]string{"X-Toolgate-Session": "session-1"}
			if test.authorize != "" {
				headers["Authorization"] = test.authorize
			}

			response := f.do(t, test.token(f.provider), headers)

			assert.Equal(t, http.StatusUnauthorized, response.Code)
			assert.Equal(t, test.want, response.Header().Get("WWW-Authenticate"))
		})
	}
}

// Routes other than /mcp keep the plain challenge, with or without a
// resource metadata URL.
func TestOtherRoutesKeepThePlainChallenge(t *testing.T) {
	t.Parallel()

	f := newFixture(t, auth.Options{ResourceMetadataURL: metadataURL}, false)

	response := f.do(t, "", nil)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.Equal(t, "Bearer", response.Header().Get("WWW-Authenticate"))

	response = f.do(t, "not-a-jwt", nil)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.Equal(t, "Bearer", response.Header().Get("WWW-Authenticate"))
}

// Without a resource metadata URL, /mcp sends the plain challenge.
func TestMCPRefusalWithoutResourceMetadata(t *testing.T) {
	t.Parallel()

	f := newFixture(t, auth.Options{}, true)

	response := f.do(t, "not-a-jwt", map[string]string{"X-Toolgate-Session": "session-1"})
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.Equal(t, "Bearer", response.Header().Get("WWW-Authenticate"))
}

// A connector's token is verified as every user token: its audience, the
// connector's client ID, must be one of the configured audiences.
func TestConnectorTokenNeedsItsAudience(t *testing.T) {
	t.Parallel()

	t.Run("audience not listed", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, auth.Options{ResourceMetadataURL: metadataURL}, true)
		token := f.provider.MintAccessToken(t, tgtestutil.TokenClaims{
			Subject:  "alice",
			Audience: []string{f.provider.ConnectorClientID},
		})

		response := f.do(t, token, map[string]string{"X-Toolgate-Session": "session-1"})
		assert.Equal(t, http.StatusUnauthorized, response.Code)

		refusals := f.recorder.all()
		require.Len(t, refusals, 1)
		assert.Equal(t, "audience not allowed", refusals[0].Reason)
		assert.Equal(t, []string{f.provider.ConnectorClientID}, refusals[0].Audience)
	})

	t.Run("audience listed", func(t *testing.T) {
		t.Parallel()

		provider := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{})
		authenticator, err := auth.New(t.Context(), auth.Options{
			IssuerURL:           provider.Issuer,
			Audiences:           []string{provider.Audience, provider.ConnectorClientID},
			ResourceMetadataURL: metadataURL,
		})
		require.NoError(t, err)

		handler := authenticator.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			identity, ok := auth.IdentityFrom(request.Context())
			assert.True(t, ok)
			assert.Equal(t, "alice", identity.Principal)
		}), true)

		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
		request.Header.Set("Authorization", "Bearer "+provider.MintAccessToken(t, tgtestutil.TokenClaims{
			Subject:  "alice",
			Audience: []string{provider.ConnectorClientID},
		}))
		request.Header.Set(auth.SessionHeader, "session-1")

		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assert.Equal(t, http.StatusOK, response.Code)
	})
}
