//go:build !windows

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/telemetry/titlemetric"
)

const hookSession = "3c1a0758-ff40-4ddf-8de5-64b8b29ec323"

// titleGateway is a fake gateway that records each /v1/metrics request and
// answers with status.
type titleGateway struct {
	status   atomic.Int32
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func (g *titleGateway) seen() ([]*http.Request, [][]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return append([]*http.Request(nil), g.requests...), append([][]byte(nil), g.bodies...)
}

// titleHookSetup signs in through a token command against a fake gateway,
// turns telemetry on, and returns the gateway and a transcript path.
func titleHookSetup(t *testing.T, transcriptLines ...string) (*titleGateway, string) {
	t.Helper()

	isolateUserConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	gateway := &titleGateway{}
	gateway.status.Store(http.StatusOK)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)

		gateway.mu.Lock()
		gateway.requests = append(gateway.requests, request)
		gateway.bodies = append(gateway.bodies, body)
		gateway.mu.Unlock()

		writer.WriteHeader(int(gateway.status.Load()))
	}))
	t.Cleanup(server.Close)

	t.Setenv("TOOLGATE_GATEWAY", server.URL)
	t.Setenv("TOOLGATE_TOKEN_COMMAND", "echo "+commandJWT("hook", time.Now().Add(time.Hour)))
	t.Setenv("TOOLGATE_WORKSPACE", "dev-box")
	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "1")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")

	path := filepath.Join(t.TempDir(), hookSession+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(transcriptLines, "\n")+"\n"), 0o600))

	return gateway, path
}

func titleRecord(kind, field, title string) string {
	line, _ := json.Marshal(map[string]string{"type": kind, field: title, "sessionId": hookSession})

	return string(line)
}

func hookStdin(sessionID, transcript string) string {
	input, _ := json.Marshal(map[string]string{"session_id": sessionID, "transcript_path": transcript, "hook_event_name": "Stop"})

	return string(input)
}

// runTitleHook runs `toolgate hook <event>` and checks that it printed
// nothing and exited 0, as every run must.
func runTitleHook(t *testing.T, event, input string) {
	t.Helper()

	var stdout, stderr strings.Builder

	code := runWithInput([]string{"hook", event}, strings.NewReader(input), &stdout, &stderr)
	assert.Equal(t, 0, code)
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
}

func TestStopHookSendsTheSessionTitleToTheIngest(t *testing.T) {
	gateway, transcript := titleHookSetup(t, titleRecord("ai-title", "aiTitle", "Fix the login redirect"))

	runTitleHook(t, "stop", hookStdin(hookSession, transcript))

	requests, bodies := gateway.seen()
	require.Len(t, requests, 1)
	assert.Equal(t, http.MethodPost, requests[0].Method)
	assert.Equal(t, "/v1/metrics", requests[0].URL.Path)
	assert.Equal(t, "application/x-protobuf", requests[0].Header.Get("Content-Type"))
	assert.Equal(t, "dev-box", requests[0].Header.Get(auth.WorkspaceHeader))
	assert.True(t, strings.HasPrefix(requests[0].Header.Get("Authorization"), "Bearer "))

	request := pmetricotlp.NewExportRequest()
	require.NoError(t, request.UnmarshalProto(bodies[0]))

	resource := request.Metrics().ResourceMetrics()
	require.Equal(t, 1, resource.Len())

	service, _ := resource.At(0).Resource().Attributes().Get("service.name")
	assert.Equal(t, titlemetric.ServiceName, service.Str())

	metrics := resource.At(0).ScopeMetrics().At(0).Metrics()
	require.Equal(t, 1, metrics.Len())
	assert.Equal(t, titlemetric.Name, metrics.At(0).Name())

	points := metrics.At(0).Gauge().DataPoints()
	require.Equal(t, 1, points.Len())
	assert.EqualValues(t, 1, points.At(0).IntValue())
	assert.Equal(t, map[string]any{
		titlemetric.AttributeSessionID: hookSession,
		titlemetric.AttributeTitle:     "Fix the login redirect",
	}, points.At(0).Attributes().AsRaw())
}

func TestSessionEndHookSendsARenameMadeWithoutAnotherTurn(t *testing.T) {
	gateway, transcript := titleHookSetup(t, titleRecord("ai-title", "aiTitle", "Generated"))

	runTitleHook(t, "stop", hookStdin(hookSession, transcript))

	file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString(titleRecord("custom-title", "customTitle", "My name") + "\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	runTitleHook(t, "session-end", hookStdin(hookSession, transcript))

	_, bodies := gateway.seen()
	require.Len(t, bodies, 2)

	request := pmetricotlp.NewExportRequest()
	require.NoError(t, request.UnmarshalProto(bodies[1]))

	title, _ := request.Metrics().ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints().At(0).Attributes().Get(titlemetric.AttributeTitle)
	assert.Equal(t, "My name", title.Str())
}

func TestTitleHooksSendNothingWhenTelemetryIsOff(t *testing.T) {
	gateway, transcript := titleHookSetup(t, titleRecord("ai-title", "aiTitle", "Fix the login redirect"))

	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "0")
	runTitleHook(t, "stop", hookStdin(hookSession, transcript))

	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "1")
	t.Setenv("OTEL_METRICS_EXPORTER", "console")
	runTitleHook(t, "stop", hookStdin(hookSession, transcript))

	requests, _ := gateway.seen()
	assert.Empty(t, requests)
}

func TestTitleHooksSendNothingWithoutATitleOrAUUIDSession(t *testing.T) {
	gateway, transcript := titleHookSetup(t, `{"type":"user","sessionId":"`+hookSession+`"}`)

	runTitleHook(t, "stop", hookStdin(hookSession, transcript))
	runTitleHook(t, "stop", hookStdin("../not-a-uuid", transcript))
	runTitleHook(t, "stop", hookStdin(hookSession, filepath.Join(t.TempDir(), "missing.jsonl")))
	runTitleHook(t, "stop", "not json")

	requests, _ := gateway.seen()
	assert.Empty(t, requests)
}

func TestStopHookSendsAnUnchangedTitleOnlyAfterAnHour(t *testing.T) {
	gateway, transcript := titleHookSetup(t, titleRecord("ai-title", "aiTitle", "Fix the login redirect"))
	start := time.Now()

	sendSessionTitle(t.Context(), strings.NewReader(hookStdin(hookSession, transcript)), start)
	sendSessionTitle(t.Context(), strings.NewReader(hookStdin(hookSession, transcript)), start.Add(30*time.Minute))

	requests, _ := gateway.seen()
	assert.Len(t, requests, 1, "unchanged and sent half an hour ago")

	sendSessionTitle(t.Context(), strings.NewReader(hookStdin(hookSession, transcript)), start.Add(61*time.Minute))

	requests, _ = gateway.seen()
	assert.Len(t, requests, 2, "sent again after an hour")
}

func TestStopHookTriesAFailedSendAgainAtTheNextTurn(t *testing.T) {
	gateway, transcript := titleHookSetup(t, titleRecord("ai-title", "aiTitle", "Fix the login redirect"))

	gateway.status.Store(http.StatusServiceUnavailable)
	runTitleHook(t, "stop", hookStdin(hookSession, transcript))

	gateway.status.Store(http.StatusOK)
	runTitleHook(t, "stop", hookStdin(hookSession, transcript))
	runTitleHook(t, "stop", hookStdin(hookSession, transcript))

	requests, _ := gateway.seen()
	assert.Len(t, requests, 2, "the failed send is tried again; the sent title isn't")
}

func TestStopHookRetriesOnceAfterA401AndStaysQuiet(t *testing.T) {
	gateway, transcript := titleHookSetup(t, titleRecord("ai-title", "aiTitle", "Fix the login redirect"))

	gateway.status.Store(http.StatusUnauthorized)
	runTitleHook(t, "stop", hookStdin(hookSession, transcript))

	requests, _ := gateway.seen()
	assert.Len(t, requests, 2, "one refresh and one retry")
}

func TestStopHookGivesUpOnASlowGatewayWithinItsTimeout(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))

	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	t.Setenv("TOOLGATE_GATEWAY", server.URL)
	t.Setenv("TOOLGATE_TOKEN_COMMAND", "echo "+commandJWT("hook", time.Now().Add(time.Hour)))
	t.Setenv("TOOLGATE_WORKSPACE", "dev-box")
	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "1")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")

	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(titleRecord("ai-title", "aiTitle", "Slow")+"\n"), 0o600))

	started := time.Now()

	runTitleHook(t, "stop", hookStdin(hookSession, transcript))

	assert.Less(t, time.Since(started), titleSendTimeout+2*time.Second)
}
