//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// telemetryFixture is the recorded Claude Code session with every OTEL_LOG_*
// content variable on.
const telemetryFixture = "../fixtures/otlp/content-on"

// contentAttributes are the attributes the ingest removes by default,
// as the design lists them.
var contentAttributes = []string{
	"prompt", "prompt_text", "user_prompt", "response", "response.model_output", "new_context",
	"system_reminders", "system_prompt_preview", "user_system_prompt", "tool_parameters", "tool_input",
	"full_command", "file_path", "bash_command", "diff", "content", "output", "hook_definitions", "hook_matcher",
}

// clientIdentity is what a client sets on every record to pose as someone
// else.
var clientIdentity = map[string]string{
	"user.id": "mallory", "user_id": "mallory", "enduser.id": "mallory", "user.email": "mallory@example.com",
	"workspace.id": "claimed/other", "workspace-id": "claimed/other",
}

// replayedTelemetry is what a replay sent.
type replayedTelemetry struct {
	user      string
	workspace string
	// lastSample is the time of the newest metric sample.
	lastSample time.Time
}

func telemetryEnv(t *testing.T, name string) string {
	t.Helper()

	value := os.Getenv(name)
	if value == "" {
		t.Skipf("%s is not set: no telemetry test backend (run through scripts/e2e/run.sh)", name)
	}

	return value
}

// shiftMetrics moves every sample time by offset.
func shiftMetrics(metrics pmetric.Metrics, offset time.Duration) {
	shift := func(stamp pcommon.Timestamp) pcommon.Timestamp {
		return pcommon.NewTimestampFromTime(stamp.AsTime().Add(offset))
	}

	for _, resource := range metrics.ResourceMetrics().All() {
		for _, scope := range resource.ScopeMetrics().All() {
			for _, metric := range scope.Metrics().All() {
				if metric.Type() != pmetric.MetricTypeSum {
					continue
				}

				for _, point := range metric.Sum().DataPoints().All() {
					point.SetStartTimestamp(shift(point.StartTimestamp()))
					point.SetTimestamp(shift(point.Timestamp()))
					putClient(point.Attributes())
				}
			}
		}
	}
}

func putClient(attributes pcommon.Map) {
	for key, value := range clientIdentity {
		attributes.PutStr(key, value)
	}
}

// replayTelemetry sends the fixture to the gateway's ingest as user, with
// the headers `toolgate otel-headers` prints and the client identity
// attributes on every record. Times move to now: the two metric exports end
// 20 s and 5 s ago.
func replayTelemetry(t *testing.T, user string) replayedTelemetry {
	t.Helper()

	admin := adminClient(t)
	base := strings.TrimSuffix(forwardToGateway(t, admin, gatewayPods(t, admin)[0]), "/mcp")
	workspace := "w" + randomHex(t, 4)

	// The headers `toolgate otel-headers` prints.
	header := http.Header{}
	header.Set("Authorization", "Bearer "+oidcProvider(t).MintAccessToken(t, testutil.TokenClaims{
		Subject: user, Email: user + "@example.com", EmailVerified: true,
	}))
	header.Set(auth.WorkspaceHeader, workspace)

	files, err := filepath.Glob(filepath.Join(telemetryFixture, "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	now := time.Now()
	metricExports := 0
	result := replayedTelemetry{user: user, workspace: "claimed/" + workspace}

	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)

		var signal string

		var body []byte

		switch {
		case strings.HasSuffix(file, "-metrics.json"):
			request := pmetricotlp.NewExportRequest()
			require.NoError(t, request.UnmarshalJSON(data))

			// Both exports are cumulative points of the same series.
			target := now.Add(-20 * time.Second)
			if metricExports > 0 {
				target = now.Add(-5 * time.Second)
				result.lastSample = target
			}

			metricExports++

			last := time.Unix(0, int64(request.Metrics().ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints().At(0).Timestamp()))
			shiftMetrics(request.Metrics(), target.Sub(last))

			signal = "metrics"
			body, err = request.MarshalProto()
			require.NoError(t, err)
		case strings.HasSuffix(file, "-logs.json"):
			request := plogotlp.NewExportRequest()
			require.NoError(t, request.UnmarshalJSON(data))

			for _, resource := range request.Logs().ResourceLogs().All() {
				for _, scope := range resource.ScopeLogs().All() {
					for _, record := range scope.LogRecords().All() {
						record.SetTimestamp(pcommon.NewTimestampFromTime(now))
						record.SetObservedTimestamp(pcommon.NewTimestampFromTime(now))
						putClient(record.Attributes())
					}
				}
			}

			signal = "logs"
			body, err = request.MarshalProto()
			require.NoError(t, err)
		default:
			request := ptraceotlp.NewExportRequest()
			require.NoError(t, request.UnmarshalJSON(data))

			// New trace and span IDs would break parent links, so keep them
			// and shift the times only.
			var first time.Time

			traceIDs := map[pcommon.TraceID]pcommon.TraceID{}

			for _, resource := range request.Traces().ResourceSpans().All() {
				for _, scope := range resource.ScopeSpans().All() {
					for _, span := range scope.Spans().All() {
						if first.IsZero() || span.StartTimestamp().AsTime().Before(first) {
							first = span.StartTimestamp().AsTime()
						}
					}
				}
			}

			offset := now.Add(-time.Minute).Sub(first)
			moved := func(stamp pcommon.Timestamp) pcommon.Timestamp {
				return pcommon.NewTimestampFromTime(stamp.AsTime().Add(offset))
			}

			for _, resource := range request.Traces().ResourceSpans().All() {
				for _, scope := range resource.ScopeSpans().All() {
					for _, span := range scope.Spans().All() {
						// A trace ID of its own keeps one replay's spans apart
						// from the next one's.
						if _, found := traceIDs[span.TraceID()]; !found {
							traceIDs[span.TraceID()] = pcommon.TraceID(uuid.New())
						}

						span.SetTraceID(traceIDs[span.TraceID()])
						span.SetStartTimestamp(moved(span.StartTimestamp()))
						span.SetEndTimestamp(moved(span.EndTimestamp()))
						putClient(span.Attributes())

						for _, event := range span.Events().All() {
							event.SetTimestamp(moved(event.Timestamp()))
							putClient(event.Attributes())
						}
					}
				}
			}

			signal = "traces"
			body, err = request.MarshalProto()
			require.NoError(t, err)
		}

		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/"+signal, bytes.NewReader(body))
		require.NoError(t, err)

		request.Header = header.Clone()
		request.Header.Set("Content-Type", "application/x-protobuf")

		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)

		answer, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()

		require.Equal(t, http.StatusOK, response.StatusCode, "%s: %s", filepath.Base(file), answer)
	}

	return result
}

func getJSON(t *testing.T, address string, parameters url.Values, into any) error {
	t.Helper()

	if len(parameters) > 0 {
		address += "?" + parameters.Encode()
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
	require.NoError(t, err)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %d: %s", address, response.StatusCode, body)
	}

	return json.Unmarshal(body, into)
}

// promSeries is one series of a Prometheus instant query.
type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

func (s promSeries) float(t *testing.T) float64 {
	t.Helper()

	text, ok := s.Value[1].(string)
	require.True(t, ok)

	value, err := strconv.ParseFloat(text, 64)
	require.NoError(t, err)

	return value
}

func promQuery(t *testing.T, query string, at time.Time) ([]promSeries, error) {
	t.Helper()

	var answer struct {
		Data struct {
			Result []promSeries `json:"result"`
		} `json:"data"`
	}

	err := getJSON(t, telemetryEnv(t, "TOOLGATE_E2E_PROMETHEUS_URL")+"/api/v1/query",
		url.Values{"query": {query}, "time": {strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64)}}, &answer)

	return answer.Data.Result, err
}

// documentedQueries returns the PromQL code blocks of docs/admin/telemetry.md.
func documentedQueries(t *testing.T) []string {
	t.Helper()

	page, err := os.ReadFile("../../docs/admin/telemetry.md")
	require.NoError(t, err)

	var queries []string

	for _, block := range regexp.MustCompile("(?s)```promql\n(.*?)```").FindAllStringSubmatch(string(page), -1) {
		for line := range strings.SplitSeq(block[1], "\n") {
			if strings.TrimSpace(line) != "" {
				queries = append(queries, strings.TrimSpace(line))
			}
		}
	}

	require.Len(t, queries, 2, "docs/admin/telemetry.md must show the token and the cost query")

	return queries
}

// fixtureDeltas returns, per token type, how much the recorded cumulative
// token counter grew between the two metric exports, and the cost's growth.
func fixtureDeltas(t *testing.T) (tokens map[string]float64, cost float64) {
	t.Helper()

	load := func(name string) pmetric.Metrics {
		data, err := os.ReadFile(filepath.Join(telemetryFixture, name))
		require.NoError(t, err)

		request := pmetricotlp.NewExportRequest()
		require.NoError(t, request.UnmarshalJSON(data))

		return request.Metrics()
	}

	collect := func(metrics pmetric.Metrics) (map[string]float64, float64) {
		byType, total := map[string]float64{}, 0.0

		for _, resource := range metrics.ResourceMetrics().All() {
			for _, scope := range resource.ScopeMetrics().All() {
				for _, metric := range scope.Metrics().All() {
					for _, point := range metric.Sum().DataPoints().All() {
						switch metric.Name() {
						case "claude_code.token.usage":
							kind, _ := point.Attributes().Get("type")
							byType[kind.Str()] += point.DoubleValue() + float64(point.IntValue())
						case "claude_code.cost.usage":
							total += point.DoubleValue()
						}
					}
				}
			}
		}

		return byType, total
	}

	firstTokens, firstCost := collect(load("001-metrics.json"))
	lastTokens, lastCost := collect(load("004-metrics.json"))

	tokens = map[string]float64{}
	for kind, value := range lastTokens {
		tokens[kind] = value - firstTokens[kind]
	}

	return tokens, lastCost - firstCost
}

func TestUsageQueries(t *testing.T) {
	telemetryEnv(t, "TOOLGATE_E2E_PROMETHEUS_URL")
	telemetryEnv(t, "TOOLGATE_E2E_OTLP_FORWARD_TO")

	queries := documentedQueries(t)
	user := "usage-" + randomHex(t, 4)
	sent := replayTelemetry(t, user)
	wantTokens, wantCost := fixtureDeltas(t)
	require.Len(t, wantTokens, 4)
	require.Positive(t, wantCost)

	t.Run("the documented queries sum the fixture's tokens and cost per user, workspace and session", func(t *testing.T) {
		// increase() extrapolates a cumulative counter's growth over its
		// samples by at most half a scrape interval at each end, so the sum
		// is the growth up to a factor of 1.5.
		const slack = 1.5

		var tokenSeries, costSeries []promSeries

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			var err error

			tokenSeries, err = promQuery(t, queries[0], sent.lastSample)
			require.NoError(collect, err)

			tokenSeries = onlyUser(tokenSeries, user)
			require.Len(collect, tokenSeries, len(wantTokens))

			costSeries, err = promQuery(t, queries[1], sent.lastSample)
			require.NoError(collect, err)

			costSeries = onlyUser(costSeries, user)
			require.Len(collect, costSeries, 1)
		}, 2*time.Minute, 2*time.Second)

		for _, series := range tokenSeries {
			want, known := wantTokens[series.Metric["type"]]
			require.True(t, known, "token type %q is not in the fixture", series.Metric["type"])
			assert.Equal(t, user+"@example.com", series.Metric["user_email"])
			assert.Equal(t, sent.workspace, series.Metric["workspace_id"])
			assert.Equal(t, "8fa85861-113e-41e6-9f83-087cb2f0c06b", series.Metric["session_id"])
			assert.InDelta(t, want*(1+(slack-1)/2), series.float(t), want*(slack-1)/2+0.001, "tokens of type %s", series.Metric["type"])
		}

		assert.InDelta(t, wantCost*(1+(slack-1)/2), costSeries[0].float(t), wantCost*(slack-1)/2+1e-9, "cost")
		assert.Equal(t, sent.workspace, costSeries[0].Metric["workspace_id"])
	})
}

// onlyUser keeps the series of one user.
func onlyUser(series []promSeries, user string) []promSeries {
	var kept []promSeries

	for _, item := range series {
		if item.Metric["user_id"] == user {
			kept = append(kept, item)
		}
	}

	return kept
}

// normalizedContent is the content attribute names as Loki and Prometheus
// write labels.
func normalizedContent() map[string]bool {
	names := map[string]bool{}
	for _, name := range contentAttributes {
		names[regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(name, "_")] = true
	}

	return names
}

func TestTelemetryIdentity(t *testing.T) {
	telemetryEnv(t, "TOOLGATE_E2E_PROMETHEUS_URL")
	telemetryEnv(t, "TOOLGATE_E2E_OTLP_FORWARD_TO")

	user := "ident-" + randomHex(t, 4)
	sent := replayTelemetry(t, user)

	t.Run("a metric carries the verified principal and workspace, not the client's", func(t *testing.T) {
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			series, err := promQuery(t, `claude_code_token_usage_tokens_total{user_id="`+user+`"}`, time.Now())
			require.NoError(collect, err)
			require.NotEmpty(collect, series)

			for _, item := range series {
				assert.Equal(collect, user, item.Metric["user_id"])
				assert.Equal(collect, user, item.Metric["enduser_id"])
				assert.Equal(collect, user+"@example.com", item.Metric["user_email"])
				assert.Equal(collect, sent.workspace, item.Metric["workspace_id"])
			}
		}, 2*time.Minute, 2*time.Second)

		for _, selector := range []string{`{user_id=~".*mallory.*"}`, `{user_email=~".*mallory.*"}`, `{workspace_id=~".*other.*"}`} {
			series, err := promQuery(t, `claude_code_token_usage_tokens_total`+selector, time.Now())
			require.NoError(t, err)
			assert.Empty(t, series, selector)
		}
	})

	t.Run("a log event carries the verified principal and workspace and no content", func(t *testing.T) {
		loki := telemetryEnv(t, "TOOLGATE_E2E_LOKI_URL")
		removed := normalizedContent()

		var streams []map[string]string

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			var answer struct {
				Data struct {
					Result []struct {
						Stream map[string]string `json:"stream"`
					} `json:"result"`
				} `json:"data"`
			}

			err := getJSON(t, loki+"/loki/api/v1/query_range", url.Values{
				"query": {`{service_name="claude-code"} | user_id="` + user + `"`},
				"start": {strconv.FormatInt(time.Now().Add(-10*time.Minute).UnixNano(), 10)},
				"end":   {strconv.FormatInt(time.Now().Add(time.Minute).UnixNano(), 10)},
				"limit": {"1000"},
			}, &answer)
			require.NoError(collect, err)

			streams = nil
			for _, result := range answer.Data.Result {
				streams = append(streams, result.Stream)
			}

			require.NotEmpty(collect, streams)
		}, 2*time.Minute, 2*time.Second)

		events := map[string]bool{}

		for _, stream := range streams {
			assert.Equal(t, user, stream["user_id"])
			assert.Equal(t, user, stream["enduser_id"])
			assert.Equal(t, sent.workspace, stream["workspace_id"])
			assert.Equal(t, user+"@example.com", stream["user_email"])
			events[stream["event_name"]] = true

			for label := range stream {
				assert.False(t, removed[label], "log label %q is a removed content attribute", label)
			}
		}

		// The kept events and their names reached the backend.
		assert.True(t, events["tool_result"], "events: %v", events)
	})

	t.Run("a span carries the verified principal and workspace, no content attribute and no tool.output event", func(t *testing.T) {
		tempo := telemetryEnv(t, "TOOLGATE_E2E_TEMPO_URL")
		removed := map[string]bool{}

		for _, name := range contentAttributes {
			removed[name] = true
		}

		var traceIDs []string

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			var answer struct {
				Traces []struct {
					TraceID string `json:"traceID"`
				} `json:"traces"`
			}

			err := getJSON(t, tempo+"/api/search", url.Values{
				"q":     {`{ span.user.id = "` + user + `" }`},
				"start": {strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)},
				"end":   {strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)},
			}, &answer)
			require.NoError(collect, err)

			traceIDs = nil
			for _, found := range answer.Traces {
				traceIDs = append(traceIDs, found.TraceID)
			}

			require.NotEmpty(collect, traceIDs)
		}, 2*time.Minute, 2*time.Second)

		sawTool := false

		for _, traceID := range traceIDs {
			var trace struct {
				Batches []struct {
					ScopeSpans []struct {
						Spans []tempoSpan `json:"spans"`
					} `json:"scopeSpans"`
				} `json:"batches"`
			}

			require.NoError(t, getJSON(t, tempo+"/api/traces/"+traceID, nil, &trace))

			for _, batch := range trace.Batches {
				for _, scope := range batch.ScopeSpans {
					for _, span := range scope.Spans {
						attributes := span.attributes()
						assert.Equal(t, user, attributes["user.id"], span.Name)
						assert.Equal(t, user, attributes["enduser.id"], span.Name)
						assert.Equal(t, sent.workspace, attributes["workspace.id"], span.Name)
						assert.NotContains(t, attributes, "workspace-id")
						assert.NotContains(t, attributes, "user_id")

						for name := range attributes {
							assert.False(t, removed[name], "span %s has the removed attribute %s", span.Name, name)
						}

						for _, event := range span.Events {
							assert.NotEqual(t, "tool.output", event.Name)
						}

						sawTool = sawTool || span.Name == "claude_code.tool"
					}
				}
			}
		}

		assert.True(t, sawTool, "the claude_code.tool span did not reach the backend")
	})
}

type tempoSpan struct {
	Name       string `json:"name"`
	SpanID     string `json:"spanId"`
	Attributes []struct {
		Key   string         `json:"key"`
		Value map[string]any `json:"value"`
	} `json:"attributes"`
	Events []struct {
		Name string `json:"name"`
	} `json:"events"`
}

func (s tempoSpan) attributes() map[string]string {
	values := map[string]string{}

	for _, attribute := range s.Attributes {
		for _, value := range attribute.Value {
			values[attribute.Key] = fmt.Sprint(value)
		}
	}

	return values
}
