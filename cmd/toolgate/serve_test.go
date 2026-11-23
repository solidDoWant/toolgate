package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
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

func TestServeRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	env := testEnv{}

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("logLevel: loud\n"), 0o600))
	env.set("TOOLGATE_SHUTDOWN_TIMEOUT", "never")

	var stdout, stderr bytes.Buffer

	code := run(env.get, []string{"serve", "--config", path}, &stdout, &stderr)

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "databaseURL: required")
	assert.Contains(t, stderr.String(), `logLevel: "loud" is not one of`)
	assert.Contains(t, stderr.String(), "TOOLGATE_SHUTDOWN_TIMEOUT: must be a duration")
}

func TestServeExitsWhenTheDatabaseIsUnreachable(t *testing.T) {
	t.Parallel()

	env := testEnv{}
	env.set("TOOLGATE_LISTEN_ADDRESS", "127.0.0.1:0")
	env.set("TOOLGATE_METRICS_LISTEN_ADDRESS", "127.0.0.1:0")
	env.set("TOOLGATE_DATABASE_URL", "postgres://toolgate@127.0.0.1:1/toolgate?connect_timeout=5")
	env.set("TOOLGATE_OIDC_ISSUER_URL", "https://idp.example")
	env.set("TOOLGATE_OIDC_AUDIENCE", "toolgate")

	stdout := &lockedBuffer{}
	stderr := &lockedBuffer{}
	done := make(chan int, 1)
	ctx, stop := context.WithCancel(t.Context())

	defer stop()

	go func() { done <- runContext(ctx, env.get, []string{"serve"}, stdout, stderr) }()

	select {
	case code := <-done:
		assert.Equal(t, 1, code, "stderr: %s", stderr.String())
	case <-time.After(30 * time.Second):
		// Stop the gateway that started without its schema.
		stop()
		<-done
		require.FailNow(t, "serve kept running without its database schema", "stdout: %s", stdout.String())
	}

	assert.Contains(t, stdout.String(), "database migrations")
	assert.NotContains(t, stdout.String(), `"msg":"gateway started"`,
		"the gateway must not open its listeners before the migrations ran")
}
