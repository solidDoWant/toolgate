package ingest_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"

	"github.com/solidDoWant/toolgate/internal/telemetry/ingest"
	"github.com/solidDoWant/toolgate/internal/telemetry/titlemetric"
)

const titleSession = "3c1a0758-ff40-4ddf-8de5-64b8b29ec323"

// titleAndUsage is a request with the plugin's title gauge next to a usage
// counter, in one scope.
func titleAndUsage() pmetric.Metrics {
	metrics := titlemetric.Metrics(titleSession, "Fix the login redirect", time.Unix(100, 0))

	usage := metrics.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().AppendEmpty()
	usage.SetName("claude_code.token.usage")
	point := usage.SetEmptySum().DataPoints().AppendEmpty()
	point.SetDoubleValue(5)
	point.Attributes().PutStr("session.id", titleSession)

	return metrics
}

func forwardTitle(t *testing.T, options ingest.Options) pmetric.MetricSlice {
	t.Helper()

	fake := newBackend(t)
	options.Endpoint = fake.server.URL
	gateway := newGateway(t, options)

	response, answer := gateway.post(t, post{
		signal: "metrics", body: encode(t, pmetricotlp.NewExportRequestFromMetrics(titleAndUsage()), false),
		contentType: "application/x-protobuf", token: gateway.token(t, true),
	})
	require.Equal(t, http.StatusOK, response.StatusCode, string(answer))

	request := pmetricotlp.NewExportRequest()
	require.NoError(t, request.UnmarshalProto(fake.only(t, "/v1/metrics")))

	return request.Metrics().ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
}

func metricNames(list pmetric.MetricSlice) []string {
	names := []string{}
	for index := range list.Len() {
		names = append(names, list.At(index).Name())
	}

	return names
}

func TestIngestRemovesSessionTitlesUnlessContentIsAllowed(t *testing.T) {
	list := forwardTitle(t, ingest.Options{})

	assert.Equal(t, []string{"claude_code.token.usage"}, metricNames(list), "the title is removed; the usage is forwarded")
}

func TestIngestRemovesSessionTitlesWhenSessionIDsAreDropped(t *testing.T) {
	list := forwardTitle(t, ingest.Options{AllowContent: true, DropSessionIDFromMetrics: true})

	assert.Equal(t, []string{"claude_code.token.usage"}, metricNames(list))
}

func TestIngestForwardsStampedSessionTitlesWhenContentIsAllowed(t *testing.T) {
	list := forwardTitle(t, ingest.Options{AllowContent: true})

	require.Equal(t, []string{titlemetric.Name, "claude_code.token.usage"}, metricNames(list))

	attributes := list.At(0).Gauge().DataPoints().At(0).Attributes()

	title, found := attributes.Get(titlemetric.AttributeTitle)
	require.True(t, found)
	assert.Equal(t, "Fix the login redirect", title.Str())

	session, found := attributes.Get(titlemetric.AttributeSessionID)
	require.True(t, found)
	assert.Equal(t, titleSession, session.Str())

	_, found = attributes.Get("user.id")
	assert.True(t, found, "the verified identity is stamped on the title")

	_, found = attributes.Get("workspace.id")
	assert.True(t, found)
}

func TestIngestRemovesTheTitleAttributeFromLogsUnlessContentIsAllowed(t *testing.T) {
	for _, allow := range []bool{false, true} {
		logs := plog.NewLogs()
		record := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		record.Attributes().PutStr(titlemetric.AttributeTitle, "Fix the login redirect")
		record.Attributes().PutStr("event.name", "user_prompt")

		fake := newBackend(t)
		gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL, AllowContent: allow})

		response, answer := gateway.post(t, post{
			signal: "logs", body: encode(t, plogotlp.NewExportRequestFromLogs(logs), false),
			contentType: "application/x-protobuf", token: gateway.token(t, true),
		})
		require.Equal(t, http.StatusOK, response.StatusCode, string(answer))

		request := plogotlp.NewExportRequest()
		require.NoError(t, request.UnmarshalProto(fake.only(t, "/v1/logs")))

		forwarded := request.Logs().ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes()
		_, found := forwarded.Get(titlemetric.AttributeTitle)
		assert.Equal(t, allow, found, "allowContent=%v", allow)
	}
}
