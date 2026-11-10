// Package ingest serves the gateway's OTLP/HTTP ingest at `/v1/traces`,
// `/v1/metrics` and `/v1/logs`. It removes the
// identity attributes a client sent, sets the verified identity on every
// record, removes prompt and tool content unless allowed, and forwards the
// result synchronously to the configured OTLP endpoint. All decoding and
// encoding is done by the collector's pdata packages.
package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/solidDoWant/toolgate/internal/auth"
)

// Limits and timeouts.
const (
	// MaxBodyBytes bounds a request body, both as sent and after gzip.
	MaxBodyBytes = 4 << 20
	// MaxConcurrent is how many ingest requests decode or forward at once.
	MaxConcurrent = 4
	// ForwardTimeout bounds one forward to the backend.
	ForwardTimeout = 10 * time.Second
	// BodyTimeout bounds how long a request body may take to arrive. A slower
	// upload ends with 408 and is not decoded.
	BodyTimeout = 10 * time.Second

	busyRetryAfter        = "2"
	defaultRetryAfter     = "5"
	maxBackendRetryAfter  = 60
	maxBackendAnswerBytes = MaxBodyBytes
)

// Content types and encodings.
const (
	contentTypeProtobuf = "application/x-protobuf"
	contentTypeJSON     = "application/json"
	encodingGzip        = "gzip"
)

// Signal names, as in the route paths and the metric's signal label.
const (
	SignalTraces  = "traces"
	SignalMetrics = "metrics"
	SignalLogs    = "logs"
)

// message is a pdata OTLP export request or response.
type message interface {
	MarshalProto() ([]byte, error)
	UnmarshalProto(data []byte) error
	MarshalJSON() ([]byte, error)
	UnmarshalJSON(data []byte) error
}

// signal decodes, stamps and answers one OTLP signal.
type signal struct {
	name string
	// newRequest returns an empty request and the function that stamps it.
	newRequest  func() (message, func(stamper))
	newResponse func() message
}

var signals = []signal{
	{
		name: SignalTraces,
		newRequest: func() (message, func(stamper)) {
			request := ptraceotlp.NewExportRequest()

			return request, func(s stamper) { s.traces(request.Traces()) }
		},
		newResponse: func() message { return ptraceotlp.NewExportResponse() },
	},
	{
		name: SignalMetrics,
		newRequest: func() (message, func(stamper)) {
			request := pmetricotlp.NewExportRequest()

			return request, func(s stamper) { s.metrics(request.Metrics()) }
		},
		newResponse: func() message { return pmetricotlp.NewExportResponse() },
	},
	{
		name: SignalLogs,
		newRequest: func() (message, func(stamper)) {
			request := plogotlp.NewExportRequest()

			return request, func(s stamper) { s.logs(request.Logs()) }
		},
		newResponse: func() message { return plogotlp.NewExportResponse() },
	},
}

// Options configures the ingest.
type Options struct {
	// Endpoint is the base URL for every signal that Endpoints doesn't name.
	Endpoint string
	// Endpoints maps a signal name (SignalTraces, SignalMetrics, SignalLogs)
	// to the OTLP/HTTP base URL its data is forwarded to; the signal's
	// `/v1/<signal>` path is added to it. A signal with no endpoint is
	// accepted and discarded.
	Endpoints map[string]string
	// DropSessionIDFromMetrics removes `session.id` from metric data points.
	DropSessionIDFromMetrics bool
	// AllowContent passes prompt and tool content through.
	AllowContent bool
	// Logger receives forwarding failures. It never receives telemetry
	// content.
	Logger *slog.Logger
	// Client overrides the forwarding client. Nil uses one with a 10 s
	// timeout that follows no redirects.
	Client *http.Client
	// BodyTimeout overrides BodyTimeout. Zero uses the default.
	BodyTimeout time.Duration
}

// Ingest serves the OTLP routes. Create it with New.
type Ingest struct {
	endpoints    map[string]string
	dropSession  bool
	allowContent bool
	logger       *slog.Logger
	client       *http.Client
	slots        chan struct{}
	bodyTimeout  time.Duration
	requests     *prometheus.CounterVec
	discarded    *prometheus.CounterVec
	forwardErrs  *prometheus.CounterVec
	forwardTime  *prometheus.HistogramVec
}

// New builds the ingest.
func New(options Options) *Ingest {
	client := options.Client
	if client == nil {
		client = &http.Client{
			Timeout:   ForwardTimeout,
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	bodyTimeout := options.BodyTimeout
	if bodyTimeout <= 0 {
		bodyTimeout = BodyTimeout
	}

	endpoints := make(map[string]string, len(signals))

	for _, signal := range signals {
		endpoint, named := options.Endpoints[signal.name]
		if !named {
			endpoint = options.Endpoint
		}

		if endpoint != "" {
			endpoints[signal.name] = strings.TrimSuffix(endpoint, "/")
		}
	}

	return &Ingest{
		endpoints:    endpoints,
		dropSession:  options.DropSessionIDFromMetrics,
		allowContent: options.AllowContent,
		logger:       logger,
		client:       client,
		slots:        make(chan struct{}, MaxConcurrent),
		bodyTimeout:  bodyTimeout,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_otlp_ingest_requests_total",
			Help: "OTLP ingest requests, by signal and the HTTP status code the gateway answered.",
		}, []string{"signal", "code"}),
		discarded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_otlp_ingest_discarded_total",
			Help: "OTLP ingest requests accepted and discarded because the signal has no endpoint, by signal.",
		}, []string{"signal"}),
		forwardErrs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_otlp_ingest_forward_errors_total",
			Help: "OTLP forwards that failed or that the backend refused, by signal.",
		}, []string{"signal"}),
		forwardTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "toolgate_otlp_ingest_forward_duration_seconds",
			Help:    "Time to forward one OTLP request to its backend, by signal.",
			Buckets: prometheus.DefBuckets,
		}, []string{"signal"}),
	}
}

// Collectors returns the ingest's metrics. Register them with the gateway's
// metrics registry.
func (i *Ingest) Collectors() []prometheus.Collector {
	return []prometheus.Collector{i.requests, i.discarded, i.forwardErrs, i.forwardTime}
}

// Register serves `POST /v1/<signal>` for every signal through handle, each
// wrapped by authenticate.
func (i *Ingest) Register(handle func(pattern string, handler http.Handler),
	authenticate func(http.Handler) http.Handler,
) {
	for _, signal := range signals {
		handle("POST /v1/"+signal.name, authenticate(i.handler(signal)))
	}
}

// statusWriter records the status code for the metric.
type statusWriter struct {
	http.ResponseWriter
	code int
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}

	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}

	return w.ResponseWriter.Write(data)
}

func (i *Ingest) handler(signal signal) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder := &statusWriter{ResponseWriter: writer}
		defer func() {
			i.requests.WithLabelValues(signal.name, strconv.Itoa(recorder.code)).Inc()
		}()

		i.serve(recorder, request, signal)
	})
}

func (i *Ingest) serve(writer http.ResponseWriter, request *http.Request, signal signal) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return
	}

	json, ok := requestIsJSON(request)
	if !ok {
		http.Error(writer, "Send OTLP as application/x-protobuf or application/json.", http.StatusUnsupportedMediaType)

		return
	}

	gzipped, ok := requestIsGzip(request)
	if !ok {
		http.Error(writer, "Send OTLP with gzip or no content encoding.", http.StatusUnsupportedMediaType)

		return
	}

	body, status := i.readBody(writer, request, gzipped)
	if status != 0 {
		message := "The OTLP request body can't be read."
		if status == http.StatusRequestEntityTooLarge {
			message = "The OTLP request body is larger than 4 MiB. Send smaller batches."
		}

		if status == http.StatusRequestTimeout {
			message = "The OTLP request body did not arrive in time. Send the whole body within 10 seconds."
		}

		http.Error(writer, message, status)

		return
	}

	endpoint := i.endpoints[signal.name]
	if endpoint == "" {
		i.discarded.WithLabelValues(signal.name).Inc()
		i.answer(writer, nil, signal, nil, json)

		return
	}

	select {
	case i.slots <- struct{}{}:
		defer func() { <-i.slots }()
	default:
		writer.Header().Set("Retry-After", busyRetryAfter)
		http.Error(writer, "The gateway is busy. Retry later.", http.StatusServiceUnavailable)

		return
	}

	export, stamp := signal.newRequest()

	if err := unmarshal(export, body, json); err != nil {
		http.Error(writer, "The OTLP request body can't be decoded.", http.StatusBadRequest)

		return
	}

	stamp(stamper{identity: identity, allowContent: i.allowContent, dropSessionID: i.dropSession})

	encoded, err := export.MarshalProto()
	if err != nil {
		http.Error(writer, "The OTLP request can't be encoded.", http.StatusInternalServerError)

		return
	}

	i.forward(writer, request.Context(), signal, endpoint, encoded, json)
}

// forward sends the stamped request to the backend and maps its answer.
func (i *Ingest) forward(writer http.ResponseWriter, ctx context.Context, signal signal, endpoint string,
	body []byte, json bool,
) {
	started := time.Now()

	defer func() { i.forwardTime.WithLabelValues(signal.name).Observe(time.Since(started).Seconds()) }()

	forwardRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/"+signal.name,
		bytes.NewReader(body))
	if err != nil {
		http.Error(writer, "The OTLP backend can't be reached.", http.StatusBadGateway)

		return
	}

	forwardRequest.Header.Set("Content-Type", contentTypeProtobuf)

	response, err := i.client.Do(forwardRequest)
	if err != nil {
		i.forwardErrs.WithLabelValues(signal.name).Inc()
		i.logger.WarnContext(ctx, "forwarding OTLP data failed", "signal", signal.name, "error", err)
		unavailable(writer, "")

		return
	}

	defer func() { _ = response.Body.Close() }()

	answer, readErr := io.ReadAll(io.LimitReader(response.Body, maxBackendAnswerBytes))

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		i.forwardErrs.WithLabelValues(signal.name).Inc()
	}

	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		if readErr != nil {
			unavailable(writer, "")

			return
		}

		i.answer(writer, response, signal, answer, json)
	case response.StatusCode == http.StatusBadRequest:
		http.Error(writer, "The OTLP backend refused the data.", http.StatusBadRequest)
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode == http.StatusBadGateway,
		response.StatusCode == http.StatusServiceUnavailable, response.StatusCode == http.StatusGatewayTimeout:
		unavailable(writer, response.Header.Get("Retry-After"))
	default:
		i.logger.WarnContext(ctx, "the OTLP backend failed", "signal", signal.name, "status", response.StatusCode)
		http.Error(writer, "The OTLP backend failed.", http.StatusBadGateway)
	}
}

// answer re-encodes the backend's ExportResponse in the client's encoding.
// A nil response answers with an empty ExportResponse.
func (i *Ingest) answer(writer http.ResponseWriter, response *http.Response, signal signal, answer []byte, json bool) {
	backendJSON := false

	if response != nil {
		if mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type")); err == nil {
			backendJSON = mediaType == contentTypeJSON
		}
	}

	decoded := signal.newResponse()

	if len(answer) > 0 {
		if err := unmarshal(decoded, answer, backendJSON); err != nil {
			http.Error(writer, "The OTLP backend's answer can't be decoded.", http.StatusBadGateway)

			return
		}
	}

	var (
		encoded     []byte
		err         error
		contentType = contentTypeProtobuf
	)

	if json {
		encoded, err = decoded.MarshalJSON()
		contentType = contentTypeJSON
	} else {
		encoded, err = decoded.MarshalProto()
	}

	if err != nil {
		http.Error(writer, "The OTLP answer can't be encoded.", http.StatusInternalServerError)

		return
	}

	writer.Header().Set("Content-Type", contentType)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(encoded)
}

// unavailable answers 503 with the backend's Retry-After when it is 1 to 60
// seconds, else 5 seconds.
func unavailable(writer http.ResponseWriter, backendRetryAfter string) {
	retryAfter := defaultRetryAfter
	if seconds, err := strconv.Atoi(strings.TrimSpace(backendRetryAfter)); err == nil &&
		seconds >= 1 && seconds <= maxBackendRetryAfter {
		retryAfter = strconv.Itoa(seconds)
	}

	writer.Header().Set("Retry-After", retryAfter)
	http.Error(writer, "The OTLP backend is unavailable. Retry later.", http.StatusServiceUnavailable)
}

func unmarshal(target message, body []byte, json bool) error {
	if json {
		return target.UnmarshalJSON(body)
	}

	return target.UnmarshalProto(body)
}

// requestIsJSON reports whether the request is JSON, and whether its content
// type is one the ingest accepts.
func requestIsJSON(request *http.Request) (json, ok bool) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil {
		return false, false
	}

	switch mediaType {
	case contentTypeProtobuf:
		return false, true
	case contentTypeJSON:
		return true, true
	}

	return false, false
}

// requestIsGzip reports whether the body is gzipped, and whether its content
// encoding is one the ingest accepts.
func requestIsGzip(request *http.Request) (gzipped, ok bool) {
	values := request.Header.Values("Content-Encoding")

	switch {
	case len(values) == 0:
		return false, true
	case len(values) > 1:
		return false, false
	}

	switch strings.ToLower(strings.TrimSpace(values[0])) {
	case "", "identity":
		return false, true
	case encodingGzip:
		return true, true
	}

	return false, false
}

// readBody reads the body within MaxBodyBytes as sent and after gzip. It
// returns a non-zero status when the body is too large (413) or unreadable
// (400).
// readBody reads the request body within the body timeout. It holds no slot,
// so a slow upload costs a connection but no decoding capacity.
func (i *Ingest) readBody(writer http.ResponseWriter, request *http.Request, gzipped bool) ([]byte, int) {
	controller := http.NewResponseController(writer)
	_ = controller.SetReadDeadline(time.Now().Add(i.bodyTimeout))

	var reader io.Reader = http.MaxBytesReader(writer, request.Body, MaxBodyBytes)

	if gzipped {
		decompressor, err := gzip.NewReader(reader)
		if err != nil {
			return nil, readStatus(err)
		}

		defer func() { _ = decompressor.Close() }()

		reader = decompressor
	}

	body, err := io.ReadAll(io.LimitReader(reader, MaxBodyBytes+1))
	if err != nil {
		// On failure the deadline stays set: an expired one stops the server
		// waiting for the rest of an unread body before it sends the answer.
		return nil, readStatus(err)
	}

	// The body is complete: clear the deadline so it can't cancel the forward.
	_ = controller.SetReadDeadline(time.Time{})

	if len(body) > MaxBodyBytes {
		return nil, http.StatusRequestEntityTooLarge
	}

	return body, 0
}

func readStatus(err error) int {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return http.StatusRequestTimeout
	}

	if maxBytesError := new(http.MaxBytesError); errors.As(err, &maxBytesError) {
		return http.StatusRequestEntityTooLarge
	}

	return http.StatusBadRequest
}
