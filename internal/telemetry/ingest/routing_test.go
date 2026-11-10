package ingest_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"

	"github.com/solidDoWant/toolgate/internal/telemetry/ingest"
)

func (b *backend) paths() []string {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	paths := make([]string, 0, len(b.received))
	for path := range b.received {
		paths = append(paths, path)
	}

	return paths
}

func TestIngestSendsEachSignalToItsOwnBackend(t *testing.T) {
	metricsBackend := newBackend(t)
	logsBackend := newBackend(t)
	gateway := newGateway(t, ingest.Options{Endpoints: map[string]string{
		ingest.SignalMetrics: metricsBackend.server.URL + "/insert/0/opentelemetry/",
		ingest.SignalLogs:    logsBackend.server.URL + "/insert/opentelemetry",
	}})

	for _, signal := range signalCases() {
		response, answer := gateway.post(t, post{
			signal: signal.signal, body: encode(t, signal.request, false),
			contentType: "application/x-protobuf", token: gateway.token(t, true),
		})
		require.Equal(t, http.StatusOK, response.StatusCode, "%s: %s", signal.signal, answer)
	}

	assert.Equal(t, []string{"/insert/0/opentelemetry/v1/metrics"}, metricsBackend.paths())
	assert.Equal(t, []string{"/insert/opentelemetry/v1/logs"}, logsBackend.paths())

	// A signal with no endpoint is answered with success and counted.
	expected := `
# HELP toolgate_otlp_ingest_discarded_total OTLP ingest requests accepted and discarded because the signal has no endpoint, by signal.
# TYPE toolgate_otlp_ingest_discarded_total counter
toolgate_otlp_ingest_discarded_total{signal="traces"} 1
`
	require.NoError(t, testutil.CollectAndCompare(gateway.ingest.Collectors()[1], strings.NewReader(expected)))
}

func TestIngestDiscardsASignalWithoutEndpointInTheClientsEncoding(t *testing.T) {
	gateway := newGateway(t, ingest.Options{})

	for _, signal := range signalCases() {
		for _, json := range []bool{false, true} {
			contentType := "application/x-protobuf"
			if json {
				contentType = "application/json"
			}

			response, _ := gateway.post(t, post{
				signal: signal.signal, body: encode(t, signal.request, json),
				contentType: contentType, token: gateway.token(t, true),
			})
			assert.Equal(t, http.StatusOK, response.StatusCode, "%s json=%t", signal.signal, json)
			assert.Equal(t, contentType, response.Header.Get("Content-Type"))
		}
	}
}

func TestIngestSingleEndpointStillReceivesEverySignal(t *testing.T) {
	fake := newBackend(t)
	gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})

	for _, signal := range signalCases() {
		response, _ := gateway.post(t, post{
			signal: signal.signal, body: encode(t, signal.request, false),
			contentType: "application/x-protobuf", token: gateway.token(t, true),
		})
		require.Equal(t, http.StatusOK, response.StatusCode)
	}

	assert.ElementsMatch(t, []string{"/v1/traces", "/v1/metrics", "/v1/logs"}, fake.paths())
}

func metricPointsWithSession(sessionIDs []string, timestamps []pcommon.Timestamp, values []float64) pmetric.Metrics {
	metrics := pmetric.NewMetrics()
	scope := metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()

	sum := scope.Metrics().AppendEmpty()
	sum.SetName("claude_code.token.usage")
	sum.SetEmptySum().SetAggregationTemporality(pmetric.AggregationTemporalityDelta)

	gauge := scope.Metrics().AppendEmpty()
	gauge.SetName("level")
	gauge.SetEmptyGauge()

	histogram := scope.Metrics().AppendEmpty()
	histogram.SetName("duration")
	histogram.SetEmptyHistogram()

	for index, id := range sessionIDs {
		point := sum.Sum().DataPoints().AppendEmpty()
		point.SetTimestamp(timestamps[index])
		point.SetDoubleValue(values[index])
		point.Attributes().PutStr("model", "opus")
		point.Attributes().PutStr("session.id", id)

		gaugePoint := gauge.Gauge().DataPoints().AppendEmpty()
		gaugePoint.SetTimestamp(timestamps[index])
		gaugePoint.SetDoubleValue(values[index])
		gaugePoint.Attributes().PutStr("session.id", id)

		histogramPoint := histogram.Histogram().DataPoints().AppendEmpty()
		histogramPoint.SetTimestamp(timestamps[index])
		histogramPoint.SetCount(2)
		histogramPoint.SetSum(values[index])
		histogramPoint.ExplicitBounds().FromRaw([]float64{1})
		histogramPoint.BucketCounts().FromRaw([]uint64{1, 1})
		histogramPoint.Attributes().PutStr("session.id", id)
	}

	return metrics
}

func forwardedMetrics(t *testing.T, drop bool, metrics pmetric.Metrics) pmetric.Metrics {
	t.Helper()

	fake := newBackend(t)
	gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL, DropSessionIDFromMetrics: drop})

	response, answer := gateway.post(t, post{
		signal: "metrics", body: encode(t, pmetricotlp.NewExportRequestFromMetrics(metrics), false),
		contentType: "application/x-protobuf", token: gateway.token(t, true),
	})
	require.Equal(t, http.StatusOK, response.StatusCode, string(answer))

	request := pmetricotlp.NewExportRequest()
	require.NoError(t, request.UnmarshalProto(fake.only(t, "/v1/metrics")))

	return request.Metrics()
}

func TestIngestDropsSessionIDFromMetricsAndCombinesIdenticalPoints(t *testing.T) {
	metrics := metricPointsWithSession([]string{"s1", "s2", "s3"},
		[]pcommon.Timestamp{10, 10, 20}, []float64{2, 3, 7})

	list := forwardedMetrics(t, true, metrics).ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()

	sum := list.At(0).Sum().DataPoints()
	require.Equal(t, 2, sum.Len(), "the two points at the same time combine, the later one stays")
	assert.InDelta(t, 5, sum.At(0).DoubleValue(), 0, "values add up once")
	assert.InDelta(t, 7, sum.At(1).DoubleValue(), 0)

	gauge := list.At(1).Gauge().DataPoints()
	require.Equal(t, 2, gauge.Len())
	assert.InDelta(t, 3, gauge.At(0).DoubleValue(), 0, "the last reading wins")

	histogram := list.At(2).Histogram().DataPoints()
	require.Equal(t, 2, histogram.Len())
	assert.EqualValues(t, 4, histogram.At(0).Count())
	assert.InDelta(t, 5, histogram.At(0).Sum(), 0)
	assert.Equal(t, []uint64{2, 2}, histogram.At(0).BucketCounts().AsRaw())

	for metricIndex := range list.Len() {
		assertNoSessionID(t, list.At(metricIndex))
	}
}

func assertNoSessionID(t *testing.T, metric pmetric.Metric) {
	t.Helper()

	var maps []pcommon.Map

	switch metric.Type() {
	case pmetric.MetricTypeSum:
		for index := range metric.Sum().DataPoints().Len() {
			maps = append(maps, metric.Sum().DataPoints().At(index).Attributes())
		}
	case pmetric.MetricTypeGauge:
		for index := range metric.Gauge().DataPoints().Len() {
			maps = append(maps, metric.Gauge().DataPoints().At(index).Attributes())
		}
	case pmetric.MetricTypeHistogram:
		for index := range metric.Histogram().DataPoints().Len() {
			maps = append(maps, metric.Histogram().DataPoints().At(index).Attributes())
		}
	}

	for _, attributes := range maps {
		_, found := attributes.Get("session.id")
		assert.False(t, found, "session.id on %s", metric.Name())
	}
}

func TestIngestKeepsSessionIDOnMetricsByDefault(t *testing.T) {
	metrics := metricPointsWithSession([]string{"s1", "s2"}, []pcommon.Timestamp{10, 10}, []float64{2, 3})

	sum := forwardedMetrics(t, false, metrics).ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints()

	require.Equal(t, 2, sum.Len())

	session, found := sum.At(0).Attributes().Get("session.id")
	require.True(t, found)
	assert.Equal(t, "s1", session.Str())
}

func TestIngestKeepsSessionIDOnLogsWhenDroppedFromMetrics(t *testing.T) {
	fake := newBackend(t)
	gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL, DropSessionIDFromMetrics: true})

	response, _ := gateway.post(t, post{
		signal: "logs", body: encode(t, plogotlp.NewExportRequestFromLogs(sampleLogs()), false),
		contentType: "application/x-protobuf", token: gateway.token(t, true),
	})
	require.Equal(t, http.StatusOK, response.StatusCode)

	request := plogotlp.NewExportRequest()
	require.NoError(t, request.UnmarshalProto(fake.only(t, "/v1/logs")))

	record := request.Logs().ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	session, found := record.Attributes().Get("session.id")
	require.True(t, found)
	assert.Equal(t, "client-session", session.Str())
}
