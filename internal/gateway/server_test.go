package gateway_test

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// unreachableDatabase points at a port nothing listens on.
const unreachableDatabase = "postgres://toolgate@127.0.0.1:1/toolgate?connect_timeout=1"

func testConfig() config.Config {
	cfg := config.Default()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.MetricsListenAddress = "127.0.0.1:0"
	cfg.DatabaseURL = unreachableDatabase
	cfg.ShutdownTimeout = config.Duration(10 * time.Second)

	return cfg
}

type runningServer struct {
	server *gateway.Server
	cancel context.CancelFunc
	// wait blocks until Serve returns and returns its result.
	wait func() error
}

func (r runningServer) url(path string) string {
	return "http://" + r.server.Addr().String() + path
}

// stop cancels the server's context, as SIGTERM does, and returns Serve's
// result.
func (r runningServer) stop(t *testing.T) error {
	t.Helper()
	r.cancel()

	return r.wait()
}

func start(t *testing.T, cfg config.Config, setup func(*gateway.Server)) runningServer {
	t.Helper()

	server, err := gateway.New(t.Context(), cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	if setup != nil {
		setup(server)
	}

	require.NoError(t, server.Listen())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- server.Serve(ctx) }()

	running := runningServer{server: server, cancel: cancel, wait: sync.OnceValue(func() error { return <-done })}

	t.Cleanup(func() {
		cancel()

		_ = running.wait()
	})

	return running
}

func get(t *testing.T, url string, header http.Header) (int, string) {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)

	for name, values := range header {
		request.Header[name] = values
	}

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response.StatusCode, string(body)
}

func TestProbesWithUnreachableDatabase(t *testing.T) {
	running := start(t, testConfig(), nil)

	status, _ := get(t, running.url("/healthz"), nil)
	assert.Equal(t, http.StatusOK, status)

	status, body := get(t, running.url("/readyz"), nil)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Contains(t, body, "postgresql: unavailable")
}

func TestMetricsOnSeparatePort(t *testing.T) {
	running := start(t, testConfig(), nil)

	status, body := get(t, "http://"+running.server.MetricsAddr().String()+"/metrics", nil)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "go_goroutines")

	status, _ = get(t, running.url("/metrics"), nil)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestSpansAreChildrenOfIncomingTraceparent(t *testing.T) {
	receiver := &testutil.OTLPReceiver{}
	collector := httptest.NewServer(receiver)
	t.Cleanup(collector.Close)

	cfg := testConfig()
	cfg.OTLPEndpoint = collector.URL

	running := start(t, cfg, nil)

	const (
		traceID      = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentSpanID = "00f067aa0ba902b7"
	)

	status, _ := get(t, running.url("/healthz"), http.Header{
		"Traceparent": {"00-" + traceID + "-" + parentSpanID + "-01"},
	})
	require.Equal(t, http.StatusOK, status)

	// Shutdown flushes the batched spans to the receiver.
	require.NoError(t, running.stop(t))

	spans := receiver.Spans()
	require.NotEmpty(t, spans, "no spans were exported")

	var found bool

	for _, span := range spans {
		if strings.Contains(span.GetName(), "/healthz") {
			found = true

			assert.Equal(t, traceID, hex.EncodeToString(span.GetTraceId()))
			assert.Equal(t, parentSpanID, hex.EncodeToString(span.GetParentSpanId()))
		}
	}

	assert.True(t, found, "no span for the /healthz request among %d spans", len(spans))
}

func TestShutdownDrainsInFlightRequests(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	running := start(t, testConfig(), func(server *gateway.Server) {
		server.Handle("GET /slow", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release

			_, _ = writer.Write([]byte("finished"))
		}))
	})

	type result struct {
		status int
		body   string
		err    error
	}

	inFlight := make(chan result, 1)

	go func() {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, running.url("/slow"), nil)
		if err != nil {
			inFlight <- result{err: err}

			return
		}

		response, err := http.DefaultClient.Do(request)
		if err != nil {
			inFlight <- result{err: err}

			return
		}

		defer func() { _ = response.Body.Close() }()

		body, err := io.ReadAll(response.Body)
		inFlight <- result{status: response.StatusCode, body: string(body), err: err}
	}()

	<-entered
	running.cancel()

	// Once shutdown has begun, the listener is closed and new connections
	// are refused.
	require.Eventually(t, func() bool {
		connection, err := net.DialTimeout("tcp", running.server.Addr().String(), time.Second)
		if err != nil {
			return true
		}

		_ = connection.Close()

		return false
	}, 10*time.Second, 10*time.Millisecond, "the gateway still accepts connections after shutdown began")

	close(release)

	got := <-inFlight
	require.NoError(t, got.err)
	assert.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, "finished", got.body)

	require.NoError(t, running.wait())
}

func TestShutdownGivesUpAfterTimeout(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	t.Cleanup(func() { close(release) })

	cfg := testConfig()
	cfg.ShutdownTimeout = config.Duration(100 * time.Millisecond)

	running := start(t, cfg, func(server *gateway.Server) {
		server.Handle("GET /stuck", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(entered)
			<-release
		}))
	})

	go func() {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, running.url("/stuck"), nil)
		if err != nil {
			return
		}

		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
	}()

	<-entered

	err := running.stop(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not drain within 100ms")
}

func TestClientMetadataIsServedWithoutAuthentication(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.OIDCIssuerURL = "https://idp.example/realms/main"
	cfg.OIDCCLIClientID = "toolgate-cli"
	cfg.OIDCEmailClaim = "mail"
	running := start(t, cfg, nil)

	status, body := get(t, running.url("/.well-known/toolgate-client"), nil)
	require.Equal(t, http.StatusOK, status)
	assert.JSONEq(t, `{
		"issuer": "https://idp.example/realms/main",
		"clientID": "toolgate-cli",
		"scopes": ["openid", "email", "profile", "offline_access"],
		"emailClaim": "mail"
	}`, body)
}

func TestClientMetadataIsRateLimitedPerAddress(t *testing.T) {
	t.Parallel()

	running := start(t, testConfig(), nil)

	for range 10 {
		status, _ := get(t, running.url("/.well-known/toolgate-client"), nil)
		require.Equal(t, http.StatusOK, status)
	}

	status, _ := get(t, running.url("/.well-known/toolgate-client"), nil)
	assert.Equal(t, http.StatusTooManyRequests, status)
}

// The probes and the client metadata document are the only routes on the
// main port that answer without authentication; metrics are on their own port.
func TestOnlyProbesAndClientMetadataArePublic(t *testing.T) {
	t.Parallel()

	server, err := gateway.New(t.Context(), testConfig(), slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{
		"GET /healthz",
		"GET /readyz",
		"GET /.well-known/toolgate-client",
	}, server.PublicRoutes())
}
