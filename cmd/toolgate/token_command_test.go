//go:build !windows

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func commandJWT(id string, exp time.Time) string {
	encode := func(text string) string { return base64.RawURLEncoding.EncodeToString([]byte(text)) }

	return encode(`{"alg":"none"}`) + "." + encode(fmt.Sprintf(`{"exp":%d,"jti":%q}`, exp.Unix(), id)) + "." + encode("sig")
}

// tokenGateway answers 401 to every token in revoked and 200 otherwise, and
// records the bearer of each request.
func tokenGateway(t *testing.T, revoked map[string]bool) (string, func() []string) {
	t.Helper()

	var (
		mu      sync.Mutex
		bearers []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		bearer := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")

		mu.Lock()

		bearers = append(bearers, bearer)
		isRevoked := revoked[bearer]
		mu.Unlock()

		if isRevoked {
			http.Error(writer, "no", http.StatusUnauthorized)

			return
		}

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"sessions":[],"secrets":[],"profile":{}}`))
	}))
	t.Cleanup(server.Close)

	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), bearers...)
	}
}

func TestTokenCommandRetriesOnceOn401(t *testing.T) {
	t.Parallel()

	env := testEnv{}

	isolateUserConfig(t, env)
	env.set("XDG_RUNTIME_DIR", t.TempDir())

	exp := time.Now().Add(time.Hour)
	first, second := commandJWT("first", exp), commandJWT("second", exp)
	counter := filepath.Join(t.TempDir(), "count")
	// The first run prints the revoked token; later runs print a good one.
	command := fmt.Sprintf("if [ -e %[1]s ]; then echo %[3]s; else touch %[1]s; echo %[2]s; fi", counter, first, second)

	gatewayURL, bearers := tokenGateway(t, map[string]bool{first: true})

	env.set("TOOLGATE_GATEWAY", gatewayURL)
	env.set("TOOLGATE_TOKEN_COMMAND", command)

	code, _, stderr := runCLI(t, env, "", "sessions")
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, []string{first, second}, bearers(), "one retry with the fresh token")

	// The fresh token is now cached: the next command doesn't run the command.
	code, _, stderr = runCLI(t, env, "", "sessions")
	require.Equal(t, 0, code, stderr)
	assert.Equal(t, []string{first, second, second}, bearers())
}

func TestTokenCommandRetriesOnlyOnce(t *testing.T) {
	t.Parallel()

	env := testEnv{}

	isolateUserConfig(t, env)
	env.set("XDG_RUNTIME_DIR", t.TempDir())

	exp := time.Now().Add(time.Hour)
	bad := commandJWT("bad", exp)
	gatewayURL, bearers := tokenGateway(t, map[string]bool{bad: true})

	env.set("TOOLGATE_GATEWAY", gatewayURL)
	env.set("TOOLGATE_TOKEN_COMMAND", "echo "+bad)

	code, _, stderr := runCLI(t, env, "", "sessions")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "401")
	assert.Len(t, bearers(), 2)
}

func TestTokenCommandHeadersUseTheCachedTokenAndNeverRetry(t *testing.T) {
	t.Parallel()

	env := testEnv{}

	isolateUserConfig(t, env)
	env.set("XDG_RUNTIME_DIR", t.TempDir())
	env.set("TOOLGATE_SESSION", "session-1")
	env.set("TOOLGATE_WORKSPACE", "dev")

	token := commandJWT("t", time.Now().Add(time.Hour))
	env.set("TOOLGATE_TOKEN_COMMAND", "echo "+token)

	code, stdout, stderr := runCLI(t, env, "", "headers")
	require.Equal(t, 0, code, stderr)

	var values map[string]string
	require.NoError(t, json.Unmarshal([]byte(stdout), &values))
	assert.Equal(t, "Bearer "+token, values["Authorization"])
}

func TestTokenCommandFailureSendsNoHeaders(t *testing.T) {
	t.Parallel()

	env := testEnv{}

	isolateUserConfig(t, env)
	env.set("XDG_RUNTIME_DIR", t.TempDir())
	env.set("TOOLGATE_SESSION", "session-1")
	env.set("TOOLGATE_WORKSPACE", "dev")
	env.set("TOOLGATE_TOKEN_COMMAND", "echo 'link your account' >&2; exit 1")

	code, stdout, stderr := runCLI(t, env, "", "headers")
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "TOOLGATE_TOKEN_COMMAND exited with status 1.")
}

func TestTokenCommandWithSPIFFESocketIsRefused(t *testing.T) {
	t.Parallel()

	env := testEnv{}

	isolateUserConfig(t, env)
	env.set("TOOLGATE_SPIFFE_SOCKET", "unix:///nonexistent")
	env.set("TOOLGATE_TOKEN_COMMAND", "echo abc")

	for _, command := range [][]string{{"headers"}, {"otel-headers"}, {"servers"}, {"secret", "list"}, {"sessions"}, {"hook", "session-start"}} {
		code, stdout, stderr := runCLI(t, env, "{}", command...)
		assert.Equal(t, 1, code, "%v", command)
		assert.Empty(t, stdout, "%v", command)
		assert.Equal(t, "Set only one of TOOLGATE_SPIFFE_SOCKET and TOOLGATE_TOKEN_COMMAND.\n", stderr, "%v", command)
	}
}

func TestTokenCommandNeedsAGatewayURL(t *testing.T) {
	t.Parallel()

	env := testEnv{}

	isolateUserConfig(t, env)
	env.set("TOOLGATE_GATEWAY", "")
	env.set("TOOLGATE_TOKEN_COMMAND", "echo abc")

	code, _, stderr := runCLI(t, env, "", "sessions")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "TOOLGATE_GATEWAY")
}
