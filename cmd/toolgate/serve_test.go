package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a bytes.Buffer safe for one writer and one reader.
type lockedBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return b.buffer.String()
}

func clearConfigEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{
		"TOOLGATE_CONFIG", "TOOLGATE_LISTEN_ADDRESS", "TOOLGATE_METRICS_LISTEN_ADDRESS",
		"TOOLGATE_DATABASE_URL", "TOOLGATE_SHUTDOWN_TIMEOUT", "TOOLGATE_LOG_LEVEL", "TOOLGATE_OTLP_ENDPOINT",
		"TOOLGATE_OIDC_ISSUER_URL", "TOOLGATE_OIDC_AUDIENCE", "TOOLGATE_OIDC_EMAIL_CLAIM",
		"TOOLGATE_OIDC_EMAIL_VERIFIED_CLAIM", "TOOLGATE_OIDC_GROUPS_CLAIM", "TOOLGATE_SECRETS_KEYSET_FILE",
	} {
		t.Setenv(name, "")
	}
}

func TestServeRejectsInvalidConfiguration(t *testing.T) {
	clearConfigEnv(t)

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("logLevel: loud\n"), 0o600))
	t.Setenv("TOOLGATE_SHUTDOWN_TIMEOUT", "never")

	var stdout, stderr bytes.Buffer

	code := run([]string{"serve", "--config", path}, &stdout, &stderr)

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "databaseURL: required")
	assert.Contains(t, stderr.String(), `logLevel: "loud" is not one of`)
	assert.Contains(t, stderr.String(), "TOOLGATE_SHUTDOWN_TIMEOUT: must be a duration")
}

func TestServeStopsOnSIGTERM(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("TOOLGATE_LISTEN_ADDRESS", "127.0.0.1:0")
	t.Setenv("TOOLGATE_METRICS_LISTEN_ADDRESS", "127.0.0.1:0")
	t.Setenv("TOOLGATE_DATABASE_URL", "postgres://toolgate@127.0.0.1:1/toolgate")
	t.Setenv("TOOLGATE_OIDC_ISSUER_URL", "https://idp.example")
	t.Setenv("TOOLGATE_OIDC_AUDIENCE", "toolgate")

	stdout := &lockedBuffer{}
	stderr := &lockedBuffer{}
	done := make(chan int, 1)

	go func() { done <- run([]string{"serve"}, stdout, stderr) }()

	require.Eventually(t, func() bool {
		return strings.Contains(stdout.String(), `"msg":"gateway started"`)
	}, 30*time.Second, 10*time.Millisecond, "the gateway did not start")

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case code := <-done:
		assert.Equal(t, 0, code, "stderr: %s", stderr.String())
	case <-time.After(30 * time.Second):
		require.FailNow(t, "serve did not stop after SIGTERM")
	}

	assert.Contains(t, stdout.String(), `"msg":"gateway stopped"`)
}
