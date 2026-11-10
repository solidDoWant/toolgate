package ingest_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/telemetry/ingest"
	gatewaytestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

const (
	testSubject = "alice-subject"
	testEmail   = "alice@example.com"
	// The workspace a user identity gets without a workspace header.
	testWorkspace = "claimed/default"
)

// clientIdentityAttributes are identity attributes a client might send, under
// every spelling a metrics backend would merge with the verified ones.
var clientIdentityAttributes = map[string]string{
	"user.id":           "mallory",
	"user_id":           "mallory",
	"User-Id":           "mallory",
	"user.email":        "mallory@example.com",
	"user.account_uuid": "uuid",
	"enduser.id":        "mallory",
	"workspace.id":      "claimed/other",
	"workspace-id":      "claimed/other",
}

// putClientAttributes adds the client's identity attributes, a session ID
// and an unrelated attribute.
func putClientAttributes(attributes pcommon.Map) {
	for key, value := range clientIdentityAttributes {
		attributes.PutStr(key, value)
	}

	attributes.PutStr("session.id", "client-session")
	attributes.PutStr("kept", "yes")
}

// refusals records failed sign-ins.
type refusals struct {
	mutex sync.Mutex
	list  []auth.Refusal
}

func (r *refusals) RecordAuthRefusal(_ context.Context, refusal auth.Refusal) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.list = append(r.list, refusal)
}

func (r *refusals) count() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return len(r.list)
}

// backend is a fake OTLP backend.
type backend struct {
	server *httptest.Server

	mutex    sync.Mutex
	received map[string][][]byte

	// answer, when set, writes the response. Otherwise the backend answers
	// 200 with an empty body.
	answer func(writer http.ResponseWriter, request *http.Request)
}

func newBackend(t *testing.T) *backend {
	t.Helper()

	fake := &backend{received: map[string][][]byte{}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			writer.WriteHeader(http.StatusInternalServerError)

			return
		}

		fake.mutex.Lock()
		fake.received[request.URL.Path] = append(fake.received[request.URL.Path], body)
		answer := fake.answer
		fake.mutex.Unlock()

		if request.Header.Get("Content-Type") != "application/x-protobuf" {
			writer.WriteHeader(http.StatusUnsupportedMediaType)

			return
		}

		if answer != nil {
			answer(writer, request)

			return
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fake.server.Close)

	return fake
}

func (b *backend) setAnswer(answer func(writer http.ResponseWriter, request *http.Request)) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	b.answer = answer
}

func (b *backend) only(t *testing.T, path string) []byte {
	t.Helper()

	b.mutex.Lock()
	defer b.mutex.Unlock()

	require.Len(t, b.received[path], 1, "requests the backend got at %s", path)

	return b.received[path][0]
}

// gateway serves the ingest behind a real authenticator.
type gateway struct {
	server   *httptest.Server
	provider *gatewaytestutil.OIDCProvider
	ingest   *ingest.Ingest
	refusals *refusals
}

func newGateway(t *testing.T, options ingest.Options) *gateway {
	t.Helper()

	provider := gatewaytestutil.StartOIDCProvider(t, gatewaytestutil.OIDCOptions{})
	recorder := &refusals{}

	authenticator, err := auth.New(t.Context(), auth.Options{
		IssuerURL: provider.Issuer,
		Audiences: []string{provider.Audience},
		Recorder:  recorder,
	})
	require.NoError(t, err)

	otlp := ingest.New(options)
	mux := http.NewServeMux()
	otlp.Register(mux.Handle, func(next http.Handler) http.Handler { return authenticator.Middleware(next, false) })

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &gateway{server: server, provider: provider, ingest: otlp, refusals: recorder}
}

func (g *gateway) token(t *testing.T, emailVerified bool) string {
	t.Helper()

	return g.provider.MintAccessToken(t, gatewaytestutil.TokenClaims{
		Subject:       testSubject,
		Email:         testEmail,
		EmailVerified: emailVerified,
	})
}

type post struct {
	signal      string
	body        []byte
	contentType string
	encoding    string
	token       string
}

func (g *gateway) post(t *testing.T, request post) (*http.Response, []byte) {
	t.Helper()

	httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, g.server.URL+"/v1/"+request.signal,
		bytes.NewReader(request.body))
	require.NoError(t, err)

	httpRequest.Header.Set("Content-Type", request.contentType)

	if request.encoding != "" {
		httpRequest.Header.Set("Content-Encoding", request.encoding)
	}

	if request.token != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+request.token)
	}

	response, err := http.DefaultClient.Do(httpRequest)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response, body
}

func gzipped(t *testing.T, body []byte) []byte {
	t.Helper()

	var buffer bytes.Buffer

	writer := gzip.NewWriter(&buffer)
	_, err := writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	return buffer.Bytes()
}

type encodable interface {
	MarshalProto() ([]byte, error)
	MarshalJSON() ([]byte, error)
}

func encode(t *testing.T, message encodable, json bool) []byte {
	t.Helper()

	var (
		body []byte
		err  error
	)

	if json {
		body, err = message.MarshalJSON()
	} else {
		body, err = message.MarshalProto()
	}

	require.NoError(t, err)

	return body
}

func sampleTraces() ptrace.Traces {
	traces := ptrace.NewTraces()
	resource := traces.ResourceSpans().AppendEmpty()
	putClientAttributes(resource.Resource().Attributes())

	scope := resource.ScopeSpans().AppendEmpty()
	putClientAttributes(scope.Scope().Attributes())

	span := scope.Spans().AppendEmpty()
	span.SetName("claude_code.tool")
	putClientAttributes(span.Attributes())

	event := span.Events().AppendEmpty()
	event.SetName("some.event")
	putClientAttributes(event.Attributes())

	link := span.Links().AppendEmpty()
	putClientAttributes(link.Attributes())

	return traces
}

func sampleMetrics() pmetric.Metrics {
	metrics := pmetric.NewMetrics()
	resource := metrics.ResourceMetrics().AppendEmpty()
	putClientAttributes(resource.Resource().Attributes())

	scope := resource.ScopeMetrics().AppendEmpty()
	putClientAttributes(scope.Scope().Attributes())

	sum := scope.Metrics().AppendEmpty()
	sum.SetName("claude_code.token.usage")
	point := sum.SetEmptySum().DataPoints().AppendEmpty()
	point.SetDoubleValue(3)
	putClientAttributes(point.Attributes())
	putClientAttributes(point.Exemplars().AppendEmpty().FilteredAttributes())

	gauge := scope.Metrics().AppendEmpty()
	gauge.SetName("gauge")
	gaugePoint := gauge.SetEmptyGauge().DataPoints().AppendEmpty()
	putClientAttributes(gaugePoint.Attributes())
	putClientAttributes(gaugePoint.Exemplars().AppendEmpty().FilteredAttributes())

	histogram := scope.Metrics().AppendEmpty()
	histogram.SetName("histogram")
	histogramPoint := histogram.SetEmptyHistogram().DataPoints().AppendEmpty()
	putClientAttributes(histogramPoint.Attributes())
	putClientAttributes(histogramPoint.Exemplars().AppendEmpty().FilteredAttributes())

	exponential := scope.Metrics().AppendEmpty()
	exponential.SetName("exponential")
	exponentialPoint := exponential.SetEmptyExponentialHistogram().DataPoints().AppendEmpty()
	putClientAttributes(exponentialPoint.Attributes())
	putClientAttributes(exponentialPoint.Exemplars().AppendEmpty().FilteredAttributes())

	summary := scope.Metrics().AppendEmpty()
	summary.SetName("summary")
	putClientAttributes(summary.SetEmptySummary().DataPoints().AppendEmpty().Attributes())

	return metrics
}

func sampleLogs() plog.Logs {
	logs := plog.NewLogs()
	resource := logs.ResourceLogs().AppendEmpty()
	putClientAttributes(resource.Resource().Attributes())

	scope := resource.ScopeLogs().AppendEmpty()
	putClientAttributes(scope.Scope().Attributes())

	record := scope.LogRecords().AppendEmpty()
	record.Body().SetStr("claude_code.api_request")
	putClientAttributes(record.Attributes())

	return logs
}

// assertNoIdentity checks that attributes hold none of the client's
// identity attributes, and keeps the others.
func assertNoIdentity(t *testing.T, where string, attributes pcommon.Map) {
	t.Helper()

	for key := range clientIdentityAttributes {
		_, found := attributes.Get(key)
		assert.False(t, found, "%s still has the client's %q", where, key)
	}

	assertPassedThrough(t, where, attributes)
}

func assertPassedThrough(t *testing.T, where string, attributes pcommon.Map) {
	t.Helper()

	session, found := attributes.Get("session.id")
	assert.True(t, found, "%s lost session.id", where)
	assert.Equal(t, "client-session", session.AsString(), "%s session.id", where)

	kept, found := attributes.Get("kept")
	assert.True(t, found, "%s lost an unrelated attribute", where)
	assert.Equal(t, "yes", kept.AsString())
}

// assertStamped checks that a record has exactly the verified identity.
func assertStamped(t *testing.T, where string, attributes pcommon.Map, withEmail bool) {
	t.Helper()

	want := map[string]string{
		"user.id":      testSubject,
		"enduser.id":   testSubject,
		"workspace.id": testWorkspace,
	}
	if withEmail {
		want["user.email"] = testEmail
	}

	identity := map[string]string{}

	attributes.Range(func(key string, value pcommon.Value) bool {
		if _, client := clientIdentityAttributes[key]; client {
			identity[key] = value.AsString()
		}

		return true
	})

	assert.Equal(t, want, identity, "%s identity attributes", where)
	assertPassedThrough(t, where, attributes)
}

func checkTraces(t *testing.T, body []byte, withEmail bool) {
	t.Helper()

	request := ptraceotlp.NewExportRequest()
	require.NoError(t, request.UnmarshalProto(body))

	resource := request.Traces().ResourceSpans().At(0)
	assertNoIdentity(t, "resource", resource.Resource().Attributes())

	scope := resource.ScopeSpans().At(0)
	assertNoIdentity(t, "scope", scope.Scope().Attributes())

	span := scope.Spans().At(0)
	assertStamped(t, "span", span.Attributes(), withEmail)
	assertNoIdentity(t, "span event", span.Events().At(0).Attributes())
	assertNoIdentity(t, "span link", span.Links().At(0).Attributes())
}

func checkMetrics(t *testing.T, body []byte, withEmail bool) {
	t.Helper()

	request := pmetricotlp.NewExportRequest()
	require.NoError(t, request.UnmarshalProto(body))

	resource := request.Metrics().ResourceMetrics().At(0)
	assertNoIdentity(t, "resource", resource.Resource().Attributes())

	scope := resource.ScopeMetrics().At(0)
	assertNoIdentity(t, "scope", scope.Scope().Attributes())

	metrics := scope.Metrics()
	require.Equal(t, 5, metrics.Len())

	sum := metrics.At(0).Sum().DataPoints().At(0)
	assertStamped(t, "sum point", sum.Attributes(), withEmail)
	assertNoIdentity(t, "sum exemplar", sum.Exemplars().At(0).FilteredAttributes())

	gauge := metrics.At(1).Gauge().DataPoints().At(0)
	assertStamped(t, "gauge point", gauge.Attributes(), withEmail)
	assertNoIdentity(t, "gauge exemplar", gauge.Exemplars().At(0).FilteredAttributes())

	histogram := metrics.At(2).Histogram().DataPoints().At(0)
	assertStamped(t, "histogram point", histogram.Attributes(), withEmail)
	assertNoIdentity(t, "histogram exemplar", histogram.Exemplars().At(0).FilteredAttributes())

	exponential := metrics.At(3).ExponentialHistogram().DataPoints().At(0)
	assertStamped(t, "exponential histogram point", exponential.Attributes(), withEmail)
	assertNoIdentity(t, "exponential histogram exemplar", exponential.Exemplars().At(0).FilteredAttributes())

	assertStamped(t, "summary point", metrics.At(4).Summary().DataPoints().At(0).Attributes(), withEmail)
}

func checkLogs(t *testing.T, body []byte, withEmail bool) {
	t.Helper()

	request := plogotlp.NewExportRequest()
	require.NoError(t, request.UnmarshalProto(body))

	resource := request.Logs().ResourceLogs().At(0)
	assertNoIdentity(t, "resource", resource.Resource().Attributes())

	scope := resource.ScopeLogs().At(0)
	assertNoIdentity(t, "scope", scope.Scope().Attributes())

	assertStamped(t, "log record", scope.LogRecords().At(0).Attributes(), withEmail)
}

type signalCase struct {
	signal  string
	request encodable
	check   func(t *testing.T, body []byte, withEmail bool)
}

func signalCases() []signalCase {
	return []signalCase{
		{signal: "traces", request: ptraceotlp.NewExportRequestFromTraces(sampleTraces()), check: checkTraces},
		{signal: "metrics", request: pmetricotlp.NewExportRequestFromMetrics(sampleMetrics()), check: checkMetrics},
		{signal: "logs", request: plogotlp.NewExportRequestFromLogs(sampleLogs()), check: checkLogs},
	}
}

func TestIngestStampsVerifiedIdentity(t *testing.T) {
	encodings := []struct {
		name        string
		json        bool
		gzip        bool
		contentType string
	}{
		{name: "protobuf", contentType: "application/x-protobuf"},
		{name: "protobuf gzip", gzip: true, contentType: "application/x-protobuf"},
		{name: "json", json: true, contentType: "application/json"},
		{name: "json gzip", json: true, gzip: true, contentType: "application/json; charset=utf-8"},
	}

	for _, signal := range signalCases() {
		for _, encoding := range encodings {
			for _, withEmail := range []bool{true, false} {
				name := signal.signal + " " + encoding.name
				if !withEmail {
					name += " unverified email"
				}

				t.Run(name, func(t *testing.T) {
					fake := newBackend(t)
					gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})

					body := encode(t, signal.request, encoding.json)
					contentEncoding := ""

					if encoding.gzip {
						body = gzipped(t, body)
						contentEncoding = "gzip"
					}

					response, answer := gateway.post(t, post{
						signal: signal.signal, body: body, contentType: encoding.contentType,
						encoding: contentEncoding, token: gateway.token(t, withEmail),
					})
					require.Equal(t, http.StatusOK, response.StatusCode, string(answer))

					wantType := "application/x-protobuf"
					if encoding.json {
						wantType = "application/json"
					}

					assert.Equal(t, wantType, response.Header.Get("Content-Type"))

					signal.check(t, fake.only(t, "/v1/"+signal.signal), withEmail)
				})
			}
		}
	}
}

func TestIngestRefusesUnauthenticatedRequests(t *testing.T) {
	fake := newBackend(t)
	gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})

	for _, signal := range signalCases() {
		t.Run(signal.signal, func(t *testing.T) {
			before := gateway.refusals.count()

			response, _ := gateway.post(t, post{
				signal: signal.signal, body: encode(t, signal.request, false), contentType: "application/x-protobuf",
			})
			assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
			assert.Equal(t, before+1, gateway.refusals.count(), "no auth.refuse was recorded")

			response, _ = gateway.post(t, post{
				signal: signal.signal, body: encode(t, signal.request, false), contentType: "application/x-protobuf",
				token: "not-a-token",
			})
			assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		})
	}

	fake.mutex.Lock()
	defer fake.mutex.Unlock()

	assert.Empty(t, fake.received, "a refused request reached the backend")
}

func TestIngestCountsRequests(t *testing.T) {
	fake := newBackend(t)
	gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})

	response, _ := gateway.post(t, post{
		signal: "logs", body: encode(t, plogotlp.NewExportRequestFromLogs(sampleLogs()), false),
		contentType: "application/x-protobuf", token: gateway.token(t, true),
	})
	require.Equal(t, http.StatusOK, response.StatusCode)

	response, _ = gateway.post(t, post{
		signal: "logs", body: []byte("x"), contentType: "text/plain", token: gateway.token(t, true),
	})
	require.Equal(t, http.StatusUnsupportedMediaType, response.StatusCode)

	collectors := gateway.ingest.Collectors()
	require.NotEmpty(t, collectors)

	expected := `
# HELP toolgate_otlp_ingest_requests_total OTLP ingest requests, by signal and the HTTP status code the gateway answered.
# TYPE toolgate_otlp_ingest_requests_total counter
toolgate_otlp_ingest_requests_total{code="200",signal="logs"} 1
toolgate_otlp_ingest_requests_total{code="415",signal="logs"} 1
`
	require.NoError(t, testutil.CollectAndCompare(collectors[0], strings.NewReader(expected),
		"toolgate_otlp_ingest_requests_total"))
}

func TestIngestRejectsUnsupportedOrInvalidBodies(t *testing.T) {
	tests := []struct {
		name        string
		body        []byte
		contentType string
		encoding    string
		wantStatus  int
	}{
		{name: "unknown content type", body: []byte("{}"), contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{name: "missing content type", body: []byte("{}"), wantStatus: http.StatusUnsupportedMediaType},
		{name: "unknown encoding", body: []byte("{}"), contentType: "application/json", encoding: "br", wantStatus: http.StatusUnsupportedMediaType},
		{name: "undecodable protobuf", body: []byte{0xff, 0xff, 0xff}, contentType: "application/x-protobuf", wantStatus: http.StatusBadRequest},
		{name: "undecodable json", body: []byte("{not json"), contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "gzip header but plain body", body: []byte("{}"), contentType: "application/json", encoding: "gzip", wantStatus: http.StatusBadRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newBackend(t)
			gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})

			response, _ := gateway.post(t, post{
				signal: "traces", body: test.body, contentType: test.contentType, encoding: test.encoding,
				token: gateway.token(t, true),
			})
			assert.Equal(t, test.wantStatus, response.StatusCode)
		})
	}
}

func TestIngestBodyLimits(t *testing.T) {
	const limit = 4 << 20

	tests := []struct {
		name       string
		size       int
		gzip       bool
		wantStatus int
	}{
		// A body of the limit is read in full and fails to decode.
		{name: "exactly 4 MiB sent", size: limit, wantStatus: http.StatusBadRequest},
		{name: "one byte over 4 MiB sent", size: limit + 1, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "exactly 4 MiB after gzip", size: limit, gzip: true, wantStatus: http.StatusBadRequest},
		{name: "one byte over 4 MiB after gzip", size: limit + 1, gzip: true, wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newBackend(t)
			gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})

			body := bytes.Repeat([]byte{0xff}, test.size)
			encoding := ""

			if test.gzip {
				body = gzipped(t, body)
				encoding = "gzip"

				require.Less(t, len(body), limit, "the compressed body must be under the sent limit")
			}

			response, _ := gateway.post(t, post{
				signal: "traces", body: body, contentType: "application/x-protobuf", encoding: encoding,
				token: gateway.token(t, true),
			})
			assert.Equal(t, test.wantStatus, response.StatusCode)
		})
	}
}

func TestIngestMapsBackendAnswers(t *testing.T) {
	partial := ptraceotlp.NewExportResponse()
	partial.PartialSuccess().SetRejectedSpans(2)
	partial.PartialSuccess().SetErrorMessage("two spans dropped")

	partialBody, err := partial.MarshalProto()
	require.NoError(t, err)

	status := func(code int, retryAfter string) func(http.ResponseWriter, *http.Request) {
		return func(writer http.ResponseWriter, _ *http.Request) {
			if retryAfter != "" {
				writer.Header().Set("Retry-After", retryAfter)
			}

			writer.WriteHeader(code)
		}
	}

	tests := []struct {
		name           string
		answer         func(http.ResponseWriter, *http.Request)
		wantStatus     int
		wantRetryAfter string
	}{
		{name: "200", answer: status(http.StatusOK, ""), wantStatus: http.StatusOK},
		{name: "202", answer: status(http.StatusAccepted, ""), wantStatus: http.StatusOK},
		{name: "400", answer: status(http.StatusBadRequest, ""), wantStatus: http.StatusBadRequest},
		{name: "429 with Retry-After 1", answer: status(http.StatusTooManyRequests, "1"), wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "1"},
		{name: "502 with Retry-After 60", answer: status(http.StatusBadGateway, "60"), wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "60"},
		{name: "503 with Retry-After 61", answer: status(http.StatusServiceUnavailable, "61"), wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "5"},
		{name: "503 with Retry-After 0", answer: status(http.StatusServiceUnavailable, "0"), wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "5"},
		{name: "504 without Retry-After", answer: status(http.StatusGatewayTimeout, ""), wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "5"},
		{name: "503 with a date", answer: status(http.StatusServiceUnavailable, "Wed, 21 Oct 2026 07:28:00 GMT"), wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "5"},
		{name: "500", answer: status(http.StatusInternalServerError, ""), wantStatus: http.StatusBadGateway},
		{name: "404", answer: status(http.StatusNotFound, ""), wantStatus: http.StatusBadGateway},
		{name: "redirect is not followed", answer: func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, "/elsewhere", http.StatusFound)
		}, wantStatus: http.StatusBadGateway},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newBackend(t)
			fake.setAnswer(test.answer)
			gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})

			response, _ := gateway.post(t, post{
				signal: "traces", body: encode(t, ptraceotlp.NewExportRequestFromTraces(sampleTraces()), false),
				contentType: "application/x-protobuf", token: gateway.token(t, true),
			})
			assert.Equal(t, test.wantStatus, response.StatusCode)
			assert.Equal(t, test.wantRetryAfter, response.Header.Get("Retry-After"))

			fake.mutex.Lock()
			defer fake.mutex.Unlock()

			assert.Len(t, fake.received["/v1/traces"], 1, "the gateway must forward once and not retry")
			assert.Empty(t, fake.received["/elsewhere"], "the gateway followed a redirect")
		})
	}

	t.Run("partial success in the client's encoding", func(t *testing.T) {
		for _, json := range []bool{false, true} {
			fake := newBackend(t)
			fake.setAnswer(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/x-protobuf")
				_, _ = writer.Write(partialBody)
			})
			gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})

			contentType := "application/x-protobuf"
			if json {
				contentType = "application/json"
			}

			response, body := gateway.post(t, post{
				signal: "traces", body: encode(t, ptraceotlp.NewExportRequestFromTraces(sampleTraces()), json),
				contentType: contentType, token: gateway.token(t, true),
			})
			require.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, contentType, response.Header.Get("Content-Type"))

			decoded := ptraceotlp.NewExportResponse()
			if json {
				require.NoError(t, decoded.UnmarshalJSON(body))
			} else {
				require.NoError(t, decoded.UnmarshalProto(body))
			}

			assert.Equal(t, int64(2), decoded.PartialSuccess().RejectedSpans())
			assert.Equal(t, "two spans dropped", decoded.PartialSuccess().ErrorMessage())
		}
	})

	t.Run("backend down", func(t *testing.T) {
		fake := newBackend(t)
		endpoint := fake.server.URL
		fake.server.Close()

		gateway := newGateway(t, ingest.Options{Endpoint: endpoint})

		response, _ := gateway.post(t, post{
			signal: "traces", body: encode(t, ptraceotlp.NewExportRequestFromTraces(sampleTraces()), false),
			contentType: "application/x-protobuf", token: gateway.token(t, true),
		})
		assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
		assert.Equal(t, "5", response.Header.Get("Retry-After"))
	})
}

func TestIngestRefusesFifthConcurrentRequest(t *testing.T) {
	const concurrent = 4

	fake := newBackend(t)
	arrived := make(chan struct{}, concurrent+1)
	release := make(chan struct{})

	fake.setAnswer(func(writer http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}

		<-release
		writer.WriteHeader(http.StatusOK)
	})

	gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL})
	body := encode(t, plogotlp.NewExportRequestFromLogs(sampleLogs()), false)
	token := gateway.token(t, true)

	var group sync.WaitGroup

	statuses := make(chan int, concurrent)

	for range concurrent {
		group.Go(func() {
			response, _ := gateway.post(t, post{signal: "logs", body: body, contentType: "application/x-protobuf", token: token})
			statuses <- response.StatusCode
		})
	}

	for range concurrent {
		select {
		case <-arrived:
		case <-time.After(30 * time.Second):
			close(release)
			t.Fatal("the first four requests did not all reach the backend")
		}
	}

	response, _ := gateway.post(t, post{signal: "traces", body: encode(t,
		ptraceotlp.NewExportRequestFromTraces(sampleTraces()), false), contentType: "application/x-protobuf", token: token})
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(t, "2", response.Header.Get("Retry-After"))

	close(release)
	group.Wait()
	close(statuses)

	for status := range statuses {
		assert.Equal(t, http.StatusOK, status)
	}

	// The slots are free again.
	response, _ = gateway.post(t, post{signal: "logs", body: body, contentType: "application/x-protobuf", token: token})
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

func TestIngestSlowBodiesDoNotHoldSlots(t *testing.T) {
	const slow = ingest.MaxConcurrent

	fake := newBackend(t)
	gateway := newGateway(t, ingest.Options{Endpoint: fake.server.URL, BodyTimeout: 500 * time.Millisecond})
	token := gateway.token(t, true)
	address := strings.TrimPrefix(gateway.server.URL, "http://")

	connections := make([]net.Conn, 0, slow)

	for range slow {
		connection, err := net.Dial("tcp", address)
		require.NoError(t, err)
		t.Cleanup(func() { _ = connection.Close() })

		_, err = fmt.Fprintf(connection, "POST /v1/logs HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n"+
			"Content-Type: application/x-protobuf\r\nContent-Length: 100000\r\n\r\n\x0a", token)
		require.NoError(t, err)

		connections = append(connections, connection)
	}

	// Another export succeeds while the slow bodies are still open.
	body := encode(t, plogotlp.NewExportRequestFromLogs(sampleLogs()), false)
	response, _ := gateway.post(t, post{signal: "logs", body: body, contentType: "application/x-protobuf", token: token})
	assert.Equal(t, http.StatusOK, response.StatusCode)

	// Each slow request ends with 408 once the bound passes, and is not forwarded.
	for _, connection := range connections {
		require.NoError(t, connection.SetReadDeadline(time.Now().Add(30*time.Second)))

		answer, err := http.ReadResponse(bufio.NewReader(connection), nil)
		require.NoError(t, err)
		assert.Equal(t, http.StatusRequestTimeout, answer.StatusCode)
		_ = answer.Body.Close()
	}

	fake.mutex.Lock()
	defer fake.mutex.Unlock()

	assert.Len(t, fake.received["/v1/logs"], 1, "only the complete export reaches the backend")
}
