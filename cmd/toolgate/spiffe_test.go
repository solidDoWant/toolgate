package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/solidDoWant/toolgate/internal/client/login"
)

// fakeWorkloadAPI is a Workload API server that hands out JWT-SVIDs.
type fakeWorkloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer

	key       *ecdsa.PrivateKey
	counter   atomic.Int64
	fail      bool
	mu        sync.Mutex
	audiences [][]string
}

func (f *fakeWorkloadAPI) FetchJWTSVID(ctx context.Context, request *workload.JWTSVIDRequest) (*workload.JWTSVIDResponse, error) {
	metadata.FromIncomingContext(ctx) //nolint:errcheck // only the call matters

	f.mu.Lock()
	f.audiences = append(f.audiences, request.GetAudience())
	f.mu.Unlock()

	if f.fail {
		return nil, context.DeadlineExceeded
	}

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: f.key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return nil, err
	}

	const id = "spiffe://allowed.test/workspace/dev"

	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Subject:  id,
		Audience: request.GetAudience(),
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		ID:       "svid-" + strconv.FormatInt(f.counter.Add(1), 10),
	}).Serialize()
	if err != nil {
		return nil, err
	}

	return &workload.JWTSVIDResponse{Svids: []*workload.JWTSVID{{SpiffeId: id, Svid: token}}}, nil
}

// startWorkloadAPI serves the fake on a Unix socket and returns its address.
func startWorkloadAPI(t *testing.T, fail bool) (string, *fakeWorkloadAPI) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	fake := &fakeWorkloadAPI{key: key, fail: fail}
	server := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(server, fake)

	shortDir, err := os.MkdirTemp("", "wl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })

	socket := filepath.Join(shortDir, "a.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	require.NoError(t, err)

	go func() { _ = server.Serve(listener) }()

	t.Cleanup(server.Stop)

	return "unix://" + socket, fake
}

// recordingGateway answers every request with an empty JSON object and
// records the Authorization header of each.
func recordingGateway(t *testing.T) (string, func() []string) {
	t.Helper()

	var (
		mu      sync.Mutex
		bearers []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()

		bearers = append(bearers, request.Header.Get("Authorization"))
		mu.Unlock()

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"sessions":[],"servers":[],"secrets":[],"profile":{}}`))
	}))
	t.Cleanup(server.Close)

	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), bearers...)
	}
}

func TestClientCommandsSendAJWTSVID(t *testing.T) {
	t.Run("with TOOLGATE_SPIFFE_SOCKET every client command sends a fresh JWT-SVID", func(t *testing.T) {
		isolateUserConfig(t)

		socket, fake := startWorkloadAPI(t, false)
		gatewayURL, bearers := recordingGateway(t)

		t.Setenv("TOOLGATE_SPIFFE_SOCKET", socket)
		t.Setenv("TOOLGATE_GATEWAY", gatewayURL)
		t.Setenv("TOOLGATE_SESSION", "session-1")
		t.Setenv("TOOLGATE_WORKSPACE", "dev")
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		t.Setenv("CLAUDE_CODE_SESSION_ID", "session-1")

		gatewayCommands := [][]string{
			{"servers"},
			{"secret", "list"},
			{"sessions"},
		}

		for _, command := range gatewayCommands {
			before := len(bearers())
			code, _, stderr := runCLI(t, "", command...)
			// `servers` speaks MCP, which the plain fake gateway doesn't, so
			// only the credential it sent is checked for that one.
			if command[0] != "servers" {
				require.Equal(t, 0, code, "%v: %s", command, stderr)
			}

			require.NotEmpty(t, bearers()[before:], "%v sends a request", command)
		}

		// `audit tail` reconnects until interrupted, so one stream is opened
		// directly.
		before := len(bearers())
		tail := tailer{stdout: &strings.Builder{}, stderr: &strings.Builder{}}
		require.NoError(t, tail.once(t.Context()))
		require.Len(t, bearers()[before:], 1)

		for _, command := range [][]string{{"headers"}, {"otel-headers"}} {
			code, stdout, stderr := runCLI(t, "", command...)
			require.Equal(t, 0, code, "%v: %s", command, stderr)

			var values map[string]string
			require.NoError(t, json.Unmarshal([]byte(stdout), &values))
			assert.True(t, strings.HasPrefix(values["Authorization"], "Bearer ey"), "%v", command)
		}

		code, _, stderr := runCLI(t, `{"session_id":"session-1"}`, "hook", "session-start")
		require.Equal(t, 0, code, stderr)

		sent := bearers()
		require.GreaterOrEqual(t, len(sent), 5, "the four commands and the hook")

		seen := map[string]bool{}

		for _, bearer := range sent {
			assert.True(t, strings.HasPrefix(bearer, "Bearer ey"), "bearer: %q", bearer)
			seen[bearer] = true
		}

		assert.Len(t, seen, len(sent), "every run fetches a fresh JWT-SVID")

		fake.mu.Lock()
		defer fake.mu.Unlock()

		assert.Len(t, fake.audiences, len(sent)+2)

		for _, audience := range fake.audiences {
			assert.Equal(t, []string{"toolgate"}, audience)
		}
	})
}

func TestSpiffeFetchFailureNeverFallsBackToTheSignIn(t *testing.T) {
	isolateUserConfig(t)

	socket, _ := startWorkloadAPI(t, true)
	gatewayURL, bearers := recordingGateway(t)

	// A saved sign-in exists but must not be used.
	settingsDir, err := configDir()
	require.NoError(t, err)
	require.NoError(t, login.SaveSettings(settingsDir, login.Settings{Gateway: gatewayURL}))

	t.Setenv("TOOLGATE_SPIFFE_SOCKET", socket)
	t.Setenv("TOOLGATE_SESSION", "session-1")
	t.Setenv("TOOLGATE_WORKSPACE", "dev")

	for _, command := range [][]string{{"servers"}, {"sessions"}, {"secret", "list"}, {"headers"}, {"otel-headers"}} {
		code, stdout, stderr := runCLI(t, "", command...)
		assert.Equal(t, 1, code, "%v", command)
		assert.Empty(t, stdout, "%v", command)
		assert.Contains(t, stderr, "JWT-SVID", "%v", command)
		assert.NotContains(t, stderr, "toolgate login", "%v", command)
	}

	assert.Empty(t, bearers(), "nothing reaches the gateway")
}

func TestSpiffeNeedsAGatewayURLForGatewayCommands(t *testing.T) {
	isolateUserConfig(t)

	socket, _ := startWorkloadAPI(t, false)

	t.Setenv("TOOLGATE_SPIFFE_SOCKET", socket)
	t.Setenv("TOOLGATE_GATEWAY", "")

	code, _, stderr := runCLI(t, "", "sessions")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "TOOLGATE_GATEWAY")

	// headers works without the gateway URL.
	t.Setenv("TOOLGATE_SESSION", "session-1")
	t.Setenv("TOOLGATE_WORKSPACE", "dev")

	code, _, stderr = runCLI(t, "", "headers")
	assert.Equal(t, 0, code, stderr)
}
