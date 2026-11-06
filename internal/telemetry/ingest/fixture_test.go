package ingest_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/solidDoWant/toolgate/internal/telemetry/ingest"
)

// fixtureDirectory holds the recorded Claude Code exports, run with every
// OTEL_LOG_* content variable on.
const fixtureDirectory = "../../../test/fixtures/otlp/content-on"

// contentCounts counts how often each content attribute and each tool.output
// span event occurs in the traces and logs.
func contentCounts(traces []ptrace.Traces, logs []plog.Logs) map[string]int {
	counts := map[string]int{}

	count := func(attributes pcommon.Map) {
		for _, name := range contentAttributeNames {
			if _, found := attributes.Get(name); found {
				counts[name]++
			}
		}
	}

	for _, export := range traces {
		for _, resource := range export.ResourceSpans().All() {
			for _, scope := range resource.ScopeSpans().All() {
				for _, span := range scope.Spans().All() {
					count(span.Attributes())

					for _, event := range span.Events().All() {
						count(event.Attributes())

						if event.Name() == "tool.output" {
							counts["event:tool.output"]++
						}
					}
				}
			}
		}
	}

	for _, export := range logs {
		for _, resource := range export.ResourceLogs().All() {
			for _, scope := range resource.ScopeLogs().All() {
				for _, record := range scope.LogRecords().All() {
					count(record.Attributes())
				}
			}
		}
	}

	return counts
}

// TestIngestPassesRecordedContentWhenAllowed replays the recorded Claude Code
// exports. The recording has content attributes on its spans and logs; with
// telemetry.allowContent they all reach the backend, except the two the
// identity prefix rule removes. Without it, none do.
func TestIngestPassesRecordedContentWhenAllowed(t *testing.T) {
	var inTraces []ptrace.Traces

	var inLogs []plog.Logs

	files, err := filepath.Glob(filepath.Join(fixtureDirectory, "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	type export struct {
		signal string
		body   []byte
	}

	var exports []export

	for _, file := range files {
		body, err := os.ReadFile(file)
		require.NoError(t, err)

		switch {
		case strings.HasSuffix(file, "-traces.json"):
			request := ptraceotlp.NewExportRequest()
			require.NoError(t, request.UnmarshalJSON(body))
			inTraces = append(inTraces, request.Traces())
			exports = append(exports, export{"traces", body})
		case strings.HasSuffix(file, "-logs.json"):
			request := plogotlp.NewExportRequest()
			require.NoError(t, request.UnmarshalJSON(body))
			inLogs = append(inLogs, request.Logs())
			exports = append(exports, export{"logs", body})
		default:
			exports = append(exports, export{"metrics", body})
		}
	}

	before := contentCounts(inTraces, inLogs)
	require.NotZero(t, before["event:tool.output"], "the recording has no tool.output event")
	require.NotZero(t, before["full_command"])
	require.NotZero(t, before["tool_parameters"])

	for _, allowContent := range []bool{true, false} {
		name := "content removed"
		if allowContent {
			name = "content allowed"
		}

		t.Run(name, func(t *testing.T) {
			fake := newBackend(t)
			gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL, AllowContent: allowContent})

			var outTraces []ptrace.Traces

			var outLogs []plog.Logs

			for _, item := range exports {
				response, body := gateway.post(t, post{
					signal: item.signal, body: item.body, contentType: "application/json", token: gateway.token(t, true),
				})
				require.Equal(t, http.StatusOK, response.StatusCode, string(body))
			}

			fake.mutex.Lock()
			for _, body := range fake.received["/v1/traces"] {
				request := ptraceotlp.NewExportRequest()
				require.NoError(t, request.UnmarshalProto(body))
				outTraces = append(outTraces, request.Traces())
			}

			for _, body := range fake.received["/v1/logs"] {
				request := plogotlp.NewExportRequest()
				require.NoError(t, request.UnmarshalProto(body))
				outLogs = append(outLogs, request.Logs())
			}
			fake.mutex.Unlock()

			after := contentCounts(outTraces, outLogs)

			for key, want := range before {
				switch {
				case !allowContent, prefixRemoved[key]:
					assert.Zero(t, after[key], "%s reached the backend", key)
				default:
					assert.Equal(t, want, after[key], "%s count", key)
				}
			}
		})
	}
}
