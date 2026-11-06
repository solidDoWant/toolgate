//go:build !windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attestationGateway answers every request with an empty JSON object and
// records the attestation header of each.
func attestationGateway(t *testing.T) (string, func() []string) {
	t.Helper()

	var (
		mu    sync.Mutex
		sents []string
	)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()

		sents = append(sents, request.Header.Get("X-Toolgate-Attestation"))
		mu.Unlock()

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"sessions":[],"secrets":[],"profile":{}}`))
	}))
	t.Cleanup(server.Close)

	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), sents...)
	}
}

func setupAttestedClient(t *testing.T) (string, func() []string) {
	t.Helper()
	isolateUserConfig(t)

	gatewayURL, sents := attestationGateway(t)

	t.Setenv("TOOLGATE_GATEWAY", gatewayURL)
	t.Setenv("TOOLGATE_TOKEN_COMMAND", "echo user-token")
	t.Setenv("TOOLGATE_SESSION", "session-1")
	t.Setenv("TOOLGATE_WORKSPACE", "dev")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-1")

	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte("workload-one\n"), 0o600))
	t.Setenv("TOOLGATE_ATTESTATION_TOKEN_FILE", file)

	return file, sents
}

func TestAttestationTokenFileIsSentByEveryGatewayCommand(t *testing.T) {
	file, sents := setupAttestedClient(t)

	for _, command := range [][]string{{"sessions"}, {"secret", "list"}} {
		code, _, stderr := runCLI(t, "", command...)
		require.Equal(t, 0, code, "%v: %s", command, stderr)
	}

	tail := tailer{stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	require.NoError(t, tail.once(t.Context()))

	code, _, stderr := runCLI(t, `{"session_id":"session-1"}`, "hook", "session-start")
	require.Equal(t, 0, code, stderr)

	for _, sent := range sents() {
		assert.Equal(t, "workload-one", sent, "raw, without Bearer, and trimmed")
	}

	assert.Len(t, sents(), 4)

	for _, command := range []string{"headers", "otel-headers"} {
		code, stdout, stderr := runCLI(t, "", command)
		require.Equal(t, 0, code, stderr)

		var values map[string]string
		require.NoError(t, json.Unmarshal([]byte(stdout), &values))
		assert.Equal(t, "workload-one", values["X-Toolgate-Attestation"], command)
		assert.Equal(t, "Bearer user-token", values["Authorization"], command)
	}

	// The file is read on each run.
	require.NoError(t, os.WriteFile(file, []byte("workload-two"), 0o600))

	code, stdout, stderr := runCLI(t, "", "headers")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "workload-two")
}

func TestAttestationFailureSendsNothing(t *testing.T) {
	file, sents := setupAttestedClient(t)
	require.NoError(t, os.Remove(file))

	for _, command := range [][]string{{"headers"}, {"otel-headers"}, {"sessions"}, {"secret", "list"}, {"servers"}} {
		code, stdout, stderr := runCLI(t, "", command...)
		assert.Equal(t, 1, code, "%v", command)
		assert.Empty(t, stdout, "%v", command)
		assert.Contains(t, stderr, "attestation token file", "%v", command)
	}

	// The hook never fails the session, but sends no request either.
	code, stdout, _ := runCLI(t, `{"session_id":"session-1"}`, "hook", "session-start")
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout, "attestation failed")

	assert.Empty(t, sents(), "no request reaches the gateway")
}

func TestAttestationVariablesConflict(t *testing.T) {
	isolateUserConfig(t)

	t.Run("two attestation variables", func(t *testing.T) {
		t.Setenv("TOOLGATE_ATTESTATION_TOKEN_FILE", "/x")
		t.Setenv("TOOLGATE_ATTESTATION_TOKEN_REQUEST", "/y")

		code, _, stderr := runCLI(t, "", "sessions")
		assert.Equal(t, 1, code)
		assert.Equal(t, "Set only one of TOOLGATE_ATTESTATION_TOKEN_FILE, TOOLGATE_ATTESTATION_SPIFFE_SOCKET and TOOLGATE_ATTESTATION_TOKEN_REQUEST.\n", stderr)
	})

	t.Run("with TOOLGATE_SPIFFE_SOCKET", func(t *testing.T) {
		t.Setenv("TOOLGATE_ATTESTATION_TOKEN_FILE", "/x")
		t.Setenv("TOOLGATE_SPIFFE_SOCKET", "unix:///nowhere")

		code, _, stderr := runCLI(t, "", "headers")
		assert.Equal(t, 1, code)
		assert.Contains(t, stderr, "TOOLGATE_SPIFFE_SOCKET")
		assert.Contains(t, stderr, "TOOLGATE_ATTESTATION_TOKEN_FILE")
	})
}

func TestAttestationSPIFFESocketFetchesAnSVID(t *testing.T) {
	_, sents := setupAttestedClient(t)
	t.Setenv("TOOLGATE_ATTESTATION_TOKEN_FILE", "")

	socket, fake := startWorkloadAPI(t, false)
	t.Setenv("TOOLGATE_ATTESTATION_SPIFFE_SOCKET", socket)

	code, _, stderr := runCLI(t, "", "sessions")
	require.Equal(t, 0, code, stderr)
	require.Len(t, sents(), 1)
	assert.True(t, strings.HasPrefix(sents()[0], "ey"))

	fake.mu.Lock()
	assert.Equal(t, [][]string{{"toolgate"}}, fake.audiences)
	fake.mu.Unlock()

	failing, _ := startWorkloadAPI(t, true)
	t.Setenv("TOOLGATE_ATTESTATION_SPIFFE_SOCKET", failing)

	code, _, stderr = runCLI(t, "", "sessions")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "attestation JWT-SVID")
	assert.Len(t, sents(), 1, "nothing was sent")
}
