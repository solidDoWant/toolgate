package ingest_test

import (
	"net/http"
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

// contentAttributeNames are the attributes Claude Code adds with
// OTEL_LOG_USER_PROMPTS, OTEL_LOG_TOOL_DETAILS and OTEL_LOG_TOOL_CONTENT.
var contentAttributeNames = []string{
	"prompt", "prompt_text", "user_prompt", "response", "response.model_output", "new_context",
	"system_reminders", "system_prompt_preview", "user_system_prompt", "tool_parameters", "tool_input",
	"full_command", "file_path", "bash_command", "diff", "content", "output", "hook_definitions", "hook_matcher",
}

// prefixRemoved are the content attributes the identity prefix rule removes
// even when content is allowed.
var prefixRemoved = map[string]bool{"user_prompt": true, "user_system_prompt": true}

// nameAttributes only name things and always pass.
var nameAttributes = []string{"skill_name", "subagent_type", "workflow.name", "server_name", "command_name"}

func putContent(attributes pcommon.Map) {
	for _, name := range contentAttributeNames {
		attributes.PutStr(name, "secret "+name)
	}

	for _, name := range nameAttributes {
		attributes.PutStr(name, "a name")
	}

	attributes.PutStr("error", "the full error")
}

func contentTraces() ptrace.Traces {
	traces := ptrace.NewTraces()
	spans := traces.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans()

	tool := spans.AppendEmpty()
	tool.SetName("claude_code.tool")
	putContent(tool.Attributes())

	output := tool.Events().AppendEmpty()
	output.SetName("tool.output")
	putContent(output.Attributes())

	other := tool.Events().AppendEmpty()
	other.SetName("other.event")
	putContent(other.Attributes())

	execution := spans.AppendEmpty()
	execution.SetName("claude_code.tool.execution")
	putContent(execution.Attributes())

	return traces
}

func contentLogs() plog.Logs {
	logs := plog.NewLogs()
	records := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()

	for _, name := range []string{"system_prompt", "tool_result", "mcp_server_connection", "api_error", "user_prompt"} {
		record := records.AppendEmpty()
		// Some records name their event only in the short `event.name`
		// attribute, as Claude Code sends it.
		if name != "system_prompt" && name != "mcp_server_connection" {
			record.Body().SetStr("claude_code." + name)
		}

		// Others name it only in their body or in the event name field.
		if name == "mcp_server_connection" {
			record.SetEventName("claude_code." + name)
		} else if name != "tool_result" {
			record.Attributes().PutStr("event.name", name)
		}

		putContent(record.Attributes())
	}

	return logs
}

// assertContent checks which content attributes attributes holds.
func assertContent(t *testing.T, where string, attributes pcommon.Map, allowContent, wantError bool) {
	t.Helper()

	for _, name := range contentAttributeNames {
		_, found := attributes.Get(name)
		assert.Equal(t, allowContent && !prefixRemoved[name], found, "%s: %s present", where, name)
	}

	for _, name := range nameAttributes {
		_, found := attributes.Get(name)
		assert.True(t, found, "%s lost the name attribute %s", where, name)
	}

	_, found := attributes.Get("error")
	assert.Equal(t, wantError, found, "%s: error present", where)
}

func TestIngestRemovesContentUnlessAllowed(t *testing.T) {
	for _, allowContent := range []bool{false, true} {
		name := "content removed"
		if allowContent {
			name = "content allowed"
		}

		t.Run(name+" traces", func(t *testing.T) {
			fake := newBackend(t)
			gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL, AllowContent: allowContent})

			response, body := gateway.post(t, post{
				signal: "traces", body: encode(t, ptraceotlp.NewExportRequestFromTraces(contentTraces()), false),
				contentType: "application/x-protobuf", token: gateway.token(t, true),
			})
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))

			request := ptraceotlp.NewExportRequest()
			require.NoError(t, request.UnmarshalProto(fake.only(t, "/v1/traces")))

			spans := request.Traces().ResourceSpans().At(0).ScopeSpans().At(0).Spans()
			require.Equal(t, 2, spans.Len())

			tool := spans.At(0)
			assertContent(t, "claude_code.tool span", tool.Attributes(), allowContent, true)

			var eventNames []string

			for index := range tool.Events().Len() {
				event := tool.Events().At(index)
				eventNames = append(eventNames, event.Name())
				assertContent(t, event.Name()+" span event", event.Attributes(), allowContent, true)
			}

			if allowContent {
				assert.Equal(t, []string{"tool.output", "other.event"}, eventNames)
			} else {
				assert.Equal(t, []string{"other.event"}, eventNames)
			}

			assertContent(t, "claude_code.tool.execution span", spans.At(1).Attributes(), allowContent, allowContent)
		})

		t.Run(name+" logs", func(t *testing.T) {
			fake := newBackend(t)
			gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL, AllowContent: allowContent})

			response, body := gateway.post(t, post{
				signal: "logs", body: encode(t, plogotlp.NewExportRequestFromLogs(contentLogs()), true),
				contentType: "application/json", token: gateway.token(t, true),
			})
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))

			request := plogotlp.NewExportRequest()
			require.NoError(t, request.UnmarshalProto(fake.only(t, "/v1/logs")))

			records := request.Logs().ResourceLogs().At(0).ScopeLogs().At(0).LogRecords()

			wantError := map[string]bool{
				"claude_code.system_prompt":         allowContent,
				"claude_code.tool_result":           allowContent,
				"claude_code.mcp_server_connection": allowContent,
				"claude_code.api_error":             true,
				"claude_code.user_prompt":           true,
			}

			var names []string

			for index := range records.Len() {
				record := records.At(index)

				recordName := record.Body().Str() + record.EventName()
				if eventName, found := record.Attributes().Get("event.name"); found {
					recordName = "claude_code." + eventName.Str()
				}

				names = append(names, recordName)
				assertContent(t, recordName, record.Attributes(), allowContent, wantError[recordName])
			}

			want := []string{"claude_code.tool_result", "claude_code.mcp_server_connection", "claude_code.api_error", "claude_code.user_prompt"}
			if allowContent {
				want = append([]string{"claude_code.system_prompt"}, want...)
			}

			assert.Equal(t, want, names)
		})
	}
}
