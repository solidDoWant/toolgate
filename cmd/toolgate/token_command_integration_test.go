//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const coderAudience = "coder-toolgate"

func TestSignInThroughTokenCommand(t *testing.T) {
	isolateUserConfig(t)

	served := startServeWithAudiences(t, coderAudience)
	defer stopServe(t, served.stderr, served.done)

	mint := func(claims testutil.TokenClaims) string {
		if claims.Audience == nil {
			claims.Audience = []string{coderAudience}
		}

		return served.provider.MintAccessToken(t, claims)
	}
	alice := testutil.TokenClaims{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"}}
	bob := testutil.TokenClaims{Subject: "bob", Email: "bob@example.com", EmailVerified: true, Groups: []string{"sales"}}

	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TOOLGATE_GATEWAY", served.url)
	t.Setenv("TOOLGATE_SESSION", "coder-session")
	t.Setenv("TOOLGATE_WORKSPACE", "dev")

	// connect opens an MCP session with the headers `toolgate headers` prints.
	connect := func(t *testing.T) *mcp.ClientSession {
		t.Helper()

		code, stdout, stderr := runCLI(t, "", "headers")
		require.Equal(t, 0, code, stderr)

		var values map[string]string
		require.NoError(t, json.Unmarshal([]byte(stdout), &values))

		header := http.Header{}
		for name, value := range values {
			header.Set(name, value)
		}

		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint: served.url + gateway.MCPPath, HTTPClient: &http.Client{Transport: headerTransport{header: header}},
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = session.Close() })

		return session
	}

	enable := func(t *testing.T, session *mcp.ClientSession) *mcp.CallToolResult {
		t.Helper()

		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: gateway.ToolEnableServer, Arguments: map[string]any{"name": "github"}})
		require.NoError(t, err)

		return result
	}

	t.Run("a token from TOOLGATE_TOKEN_COMMAND signs the CLI in as the user, with no saved sign-in", func(t *testing.T) {
		t.Setenv("TOOLGATE_TOKEN_COMMAND", "echo "+mint(alice))

		session := connect(t)

		code, stdout, stderr := runCLI(t, "", "sessions")
		require.Equal(t, 0, code, stderr)
		assert.Contains(t, stdout, "coder-session", "the session is the user's own")

		require.Eventually(t, func() bool { return len(listServerNames(t, session)) == 1 }, time.Minute, 50*time.Millisecond, "the catalog loads")

		result := enable(t, session)
		// Access passes, so the enable gets as far as the missing secret.
		assert.Contains(t, mustJSON(t, result.Content), "github-token", "a user in the group reaches the group-only server")
		assert.NotContains(t, mustJSON(t, result.Content), "isn't in your catalog")
	})

	t.Run("a user outside the group is refused the group-only server", func(t *testing.T) {
		t.Setenv("TOOLGATE_SESSION", "coder-session-bob")
		t.Setenv("TOOLGATE_TOKEN_COMMAND", "echo "+mint(bob))

		session := connect(t)

		result := enable(t, session)
		assert.Contains(t, mustJSON(t, result.Content), "isn't in your catalog", "the groups come from the token")
	})

	t.Run("tokens the gateway must refuse get 401", func(t *testing.T) {
		good := mint(alice)
		parts := strings.Split(good, ".")
		require.Len(t, parts, 3)

		flipped := parts[2][:len(parts[2])-2] + map[bool]string{true: "AA", false: "BB"}[!strings.HasSuffix(parts[2], "AA")]

		tests := map[string]string{
			"bad signature":     parts[0] + "." + parts[1] + "." + flipped,
			"expired":           mint(testutil.TokenClaims{Subject: "alice", Groups: []string{"eng"}, IssuedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}),
			"wrong issuer":      mint(testutil.TokenClaims{Subject: "alice", Groups: []string{"eng"}, Issuer: "https://other.example.com"}),
			"unlisted audience": mint(testutil.TokenClaims{Subject: "alice", Groups: []string{"eng"}, Audience: []string{"somebody-else"}}),
		}

		for name, token := range tests {
			t.Run(name, func(t *testing.T) {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, served.url+"/api/v1/sessions", nil)
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer "+token)

				response, err := http.DefaultClient.Do(request)
				require.NoError(t, err)

				_ = response.Body.Close()

				assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
			})
		}
	})
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	return string(encoded)
}
