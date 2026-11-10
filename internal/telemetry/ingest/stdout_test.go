package ingest_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"

	"github.com/solidDoWant/toolgate/internal/telemetry/ingest"
)

// lineBuffer records each Write call as one entry.
type lineBuffer struct {
	mutex  sync.Mutex
	writes []string
}

func (b *lineBuffer) Write(data []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.writes = append(b.writes, string(data))

	return len(data), nil
}

func (b *lineBuffer) lines() []string {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return append([]string(nil), b.writes...)
}

func stdoutLogs(extra int) plog.Logs {
	logs := plog.NewLogs()
	resource := logs.ResourceLogs().AppendEmpty()
	putClientAttributes(resource.Resource().Attributes())
	resource.Resource().Attributes().PutStr("service.name", "claude-code")
	resource.Resource().Attributes().PutStr("service.version", "2.1.0")
	resource.Resource().Attributes().PutStr("os.type", "linux")
	resource.Resource().Attributes().PutStr("host.arch", "amd64")
	resource.Resource().Attributes().PutStr("host.name", "laptop")

	scope := resource.ScopeLogs().AppendEmpty()

	request := scope.LogRecords().AppendEmpty()
	request.SetTimestamp(pcommon.Timestamp(1700000000000000001))
	request.SetObservedTimestamp(pcommon.Timestamp(1700000000000000002))
	request.SetSeverityText("INFO")
	request.Body().SetStr("claude_code.api_request")
	request.Attributes().PutStr("event.name", "api_request")
	request.Attributes().PutStr("model", "opus")
	request.Attributes().PutInt("input_tokens", 42)
	putClientAttributes(request.Attributes())

	prompt := scope.LogRecords().AppendEmpty()
	prompt.Attributes().PutStr("event.name", "tool_result")
	prompt.Attributes().PutStr("full_command", "rm -rf /")

	for range extra {
		scope.LogRecords().AppendEmpty().Attributes().PutStr("event.name", "api_error")
	}

	return logs
}

func postStdoutLogs(t *testing.T, gateway *gateway, logs plog.Logs) {
	t.Helper()

	response, answer := gateway.post(t, post{
		signal: "logs", body: encode(t, plogotlp.NewExportRequestFromLogs(logs), false),
		contentType: "application/x-protobuf", token: gateway.token(t, true),
	})
	require.Equal(t, http.StatusOK, response.StatusCode, string(answer))
}

func TestIngestWritesOneStdoutLineForEachLogRecord(t *testing.T) {
	output := &lineBuffer{}
	fake := newBackend(t)
	gateway := newGateway(t, ingest.Options{
		Endpoint: fake.server.URL, Endpoints: map[string]string{ingest.SignalLogs: "stdout"}, Stdout: output,
	})

	postStdoutLogs(t, gateway, stdoutLogs(1))

	lines := output.lines()
	require.Len(t, lines, 3, "one line per record, not per request")

	for _, line := range lines {
		assert.True(t, strings.HasSuffix(line, "\n"))
		assert.Equal(t, 1, strings.Count(line, "\n"))
	}

	var first map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))

	assert.Equal(t, "claude-code", first["source"])
	assert.Equal(t, "api_request", first["event"])
	assert.Equal(t, "1700000000000000001", first["time_unix_nano"])
	assert.Equal(t, "1700000000000000002", first["observed_time_unix_nano"])
	assert.Equal(t, "INFO", first["severity"])
	assert.Equal(t, "claude_code.api_request", first["body"])
	assert.Equal(t, map[string]any{
		"service.name": "claude-code", "service.version": "2.1.0", "os.type": "linux", "host.arch": "amd64",
	}, first["resource"])

	attributes, ok := first["attributes"].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, 42, attributes["input_tokens"], 0)
	assert.Equal(t, "opus", attributes["model"])
	assert.Equal(t, testEmail, attributes["user.email"])
	assert.Equal(t, testSubject, attributes["user.id"])
	assert.Equal(t, "yes", attributes["kept"])
	assert.NotEqual(t, "mallory", attributes["enduser.id"])

	assert.NotContains(t, strings.Join(lines, ""), "rm -rf")
	assert.Empty(t, fake.received, "nothing goes to an OTLP endpoint")

	expected := `
# HELP toolgate_otlp_ingest_log_records_total OTLP log records written to stdout, by result (written or failed).
# TYPE toolgate_otlp_ingest_log_records_total counter
toolgate_otlp_ingest_log_records_total{result="written"} 3
`
	require.NoError(t, testutil.CollectAndCompare(gateway.ingest.Collectors()[4], strings.NewReader(expected)))
}

func TestIngestWritesStdoutLinesWholeUnderConcurrentRequests(t *testing.T) {
	output := &lineBuffer{}
	gateway := newGateway(t, ingest.Options{
		Endpoints: map[string]string{ingest.SignalLogs: "stdout"}, Stdout: output,
	})

	big := strings.Repeat("x", 40<<10)
	logs := plog.NewLogs()
	record := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	record.Attributes().PutStr("event.name", "api_request")
	record.Attributes().PutStr("big", big)

	var group sync.WaitGroup

	for range 4 {
		group.Go(func() { postStdoutLogs(t, gateway, logs) })
	}

	group.Wait()

	lines := output.lines()
	require.Len(t, lines, 4)

	for _, line := range lines {
		assert.Greater(t, len(line), 16<<10)
		require.True(t, json.Valid(bytes.TrimSpace([]byte(line))), "each line is whole JSON")
	}
}
