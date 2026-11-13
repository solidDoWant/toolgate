package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// runHeaders runs `toolgate headers` and returns the exit code and output.
func runHeaders(t *testing.T) (int, string, string) {
	t.Helper()

	var stdout, stderr strings.Builder

	code := run([]string{"headers"}, &stdout, &stderr)

	return code, stdout.String(), stderr.String()
}

// identityThroughGateway sends a request with headers through the gateway's
// authentication and returns the identity it saw, or the response code.
func identityThroughGateway(t *testing.T, provider *testutil.OIDCProvider, headers map[string]string) (auth.Identity, int) {
	t.Helper()

	authenticator, err := auth.New(t.Context(), auth.Options{IssuerURL: provider.Issuer, Audiences: []string{provider.Audience}})
	require.NoError(t, err)

	var identity auth.Identity

	handler := authenticator.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		identity, _ = auth.IdentityFrom(request.Context())
	}), true)

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return identity, recorder.Code
}

func TestHeadersRefreshesTheAccessToken(t *testing.T) {
	t.Run("after sign-in, toolgate headers prints a working token each time without a new sign-in", func(t *testing.T) {
		isolateUserConfig(t)
		t.Setenv("TOOLGATE_SESSION", "session-1")
		t.Setenv("TOOLGATE_WORKSPACE", "laptop-widgets")

		provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
		provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
		gatewayURL := startGateway(t, provider, provider.ClientID)

		code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
		require.Equal(t, 0, code, "stderr: %s", stderr)

		// toolgate keeps no access token: each run gets a new one with the
		// stored refresh token. The second run uses the refresh token the
		// first run received if the IdP rotated it.
		tokens := map[string]bool{}

		for range 2 {
			code, stdout, stderr := runHeaders(t)
			require.Equal(t, 0, code, "stderr: %s", stderr)

			var headers map[string]string
			require.NoError(t, json.Unmarshal([]byte(stdout), &headers))
			assert.Len(t, headers, 3)
			assert.Equal(t, "session-1", headers["X-Toolgate-Session"])
			assert.Equal(t, "laptop-widgets", headers["X-Toolgate-Workspace"])
			require.True(t, strings.HasPrefix(headers["Authorization"], "Bearer "), "Authorization: %q", headers["Authorization"])

			tokens[headers["Authorization"]] = true

			identity, status := identityThroughGateway(t, provider, headers)
			require.Equal(t, http.StatusOK, status)
			assert.Equal(t, "alice", identity.Principal)
			assert.Equal(t, "session-1", identity.Session)
			assert.Equal(t, "claimed/laptop-widgets", identity.Workspace)
		}

		assert.Len(t, tokens, 2, "each run prints a new access token")
	})
}

func TestHeadersReadsTheSessionFileOfTheParentProcess(t *testing.T) {
	isolateUserConfig(t)

	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("TOOLGATE_SESSION", "")
	t.Setenv("TOOLGATE_WORKSPACE", "laptop-widgets")

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
	require.Equal(t, 0, code, "stderr: %s", stderr)

	dir := filepath.Join(runtimeDir, "toolgate")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getppid())), []byte("from-hook\n"), 0o600))

	code, stdout, stderr := runHeaders(t)
	require.Equal(t, 0, code, "stderr: %s", stderr)

	var headers map[string]string
	require.NoError(t, json.Unmarshal([]byte(stdout), &headers))
	assert.Equal(t, "from-hook", headers["X-Toolgate-Session"])
}

func TestHeadersWithoutSessionPrintsCredentialHeaders(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TOOLGATE_SESSION", "")
	t.Setenv("TOOLGATE_WORKSPACE", "laptop-widgets")

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
	require.Equal(t, 0, code, "stderr: %s", stderr)

	code, stdout, stderr := runHeaders(t)
	require.Equal(t, 0, code, "stderr: %s", stderr)
	assert.Contains(t, stderr, "no Claude Code session found")

	var headers map[string]string
	require.NoError(t, json.Unmarshal([]byte(stdout), &headers))
	assert.Len(t, headers, 2)
	assert.NotContains(t, headers, "X-Toolgate-Session")
	assert.Equal(t, "laptop-widgets", headers["X-Toolgate-Workspace"])

	identity, status := identityThroughGateway(t, provider, headers)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "alice", identity.Principal)
	assert.Empty(t, identity.Session, "the MCP endpoint ties the session to the MCP session")
}

func TestHeadersWithoutSessionOrSignIn(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TOOLGATE_SESSION", "")

	code, stdout, stderr := runHeaders(t)
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "not signed in")
}

func TestHeadersWithoutSignIn(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("TOOLGATE_SESSION", "session-1")

	code, stdout, stderr := runHeaders(t)
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "Run toolgate login.")
}

func TestOtelHeadersPrintsTokenAndWorkspaceWithoutSession(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TOOLGATE_SESSION", "")
	t.Setenv("TOOLGATE_WORKSPACE", "laptop-widgets")

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
	require.Equal(t, 0, code, "stderr: %s", stderr)

	var stdout, errOut strings.Builder

	code = run([]string{"otel-headers"}, &stdout, &errOut)
	require.Equal(t, 0, code, "stderr: %s", errOut.String())

	var headers map[string]string
	require.NoError(t, json.Unmarshal([]byte(stdout.String()), &headers))
	assert.Len(t, headers, 2)
	assert.Equal(t, "laptop-widgets", headers["X-Toolgate-Workspace"])

	// The MCP middleware needs a session; ingest does not, so add one only
	// to check the token.
	headers["X-Toolgate-Session"] = "s1"
	identity, status := identityThroughGateway(t, provider, headers)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "alice", identity.Principal)
}

func TestOtelHeadersWithoutSignIn(t *testing.T) {
	isolateUserConfig(t)

	var stdout, stderr strings.Builder

	code := run([]string{"otel-headers"}, &stdout, &stderr)
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "Run toolgate login.")
}
