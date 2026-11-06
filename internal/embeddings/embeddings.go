// Package embeddings calls an endpoint that speaks the OpenAI embeddings API
// (POST <url>/v1/embeddings) and formats vectors for pgvector, which ranks them.
// It uses the standard library only (docs/design/phase-4.md §5).
package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	// DefaultTimeout bounds one request when Options.Timeout is zero.
	DefaultTimeout = 2 * time.Second
	// BatchSize is the number of texts sent in one request.
	BatchSize = 32
	// maxResponseBytes bounds the response body.
	maxResponseBytes = 16 << 20
	// maxDimensions bounds the length of one vector.
	maxDimensions = 8192
)

// Options configure a Client.
type Options struct {
	// URL is the endpoint's base URL, such as `http://tei.tools.svc:80`.
	URL string
	// Model is the model name sent in each request.
	Model string
	// APIKey, when set, is sent as `Authorization: Bearer`.
	APIKey string
	// Timeout bounds each request. Zero means DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the HTTP client; nil means a plain client.
	HTTPClient *http.Client
	// Now returns the current time for the circuit breaker; nil means
	// time.Now.
	Now func() time.Time
}

// Client calls the embeddings endpoint.
type Client struct {
	endpoint string
	model    string
	apiKey   string
	timeout  time.Duration
	http     *http.Client
	breaker  breaker
	metrics  *Metrics
}

// Metrics counts failed and skipped calls of the model endpoints. One value
// is shared by the embeddings clients.
type Metrics struct {
	errors *prometheus.CounterVec
}

// NewMetrics returns the counter `toolgate_tool_search_embedding_errors_total`,
// labelled by use (`query` or `document`) and reason (`error`, or
// `circuit_open` for a call skipped while the circuit is open).
func NewMetrics() *Metrics {
	return &Metrics{errors: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "toolgate_tool_search_embedding_errors_total",
		Help: "Failed or skipped calls of the tool search model endpoints, by use (query or document) and reason (error or circuit_open).",
	}, []string{"use", "reason"})}
}

// Counter returns the counter of a use and reason, for tests.
func (m *Metrics) Counter(use, reason string) prometheus.Counter {
	return m.errors.WithLabelValues(use, reason)
}

func (m *Metrics) count(use, reason string) { m.errors.WithLabelValues(use, reason).Inc() }

// Collectors returns the metrics.
func (m *Metrics) Collectors() []prometheus.Collector { return []prometheus.Collector{m.errors} }

// New returns a client, or an error when the URL is not an http(s) URL or the
// model is empty.
func New(options Options, metrics ...*Metrics) (*Client, error) {
	parsed, err := url.Parse(options.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("the embeddings URL must be an http or https URL")
	}

	if options.Model == "" {
		return nil, errors.New("the embeddings model is required")
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	client := options.HTTPClient
	if client == nil {
		client = &http.Client{}
	}

	shared := NewMetrics()
	if len(metrics) > 0 && metrics[0] != nil {
		shared = metrics[0]
	}

	return &Client{
		endpoint: strings.TrimRight(options.URL, "/") + "/v1/embeddings",
		model:    options.Model,
		apiKey:   options.APIKey,
		timeout:  timeout,
		http:     client,
		breaker:  breaker{now: valueOrNow(options.Now)},
		metrics:  shared,
	}, nil
}

// Model returns the model name.
func (c *Client) Model() string { return c.model }

// Collectors returns the client's metrics.
func (c *Client) Collectors() []prometheus.Collector { return c.metrics.Collectors() }

// CountError counts a failed use of the endpoint. use is `query` or
// `document`; err says whether the call failed or was skipped.
func (c *Client) CountError(use string, err error) {
	if errors.Is(err, ErrCircuitOpen) {
		c.metrics.count(use, "circuit_open")

		return
	}

	c.metrics.count(use, "error")
}

type request struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type response struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector for each text, in order. It sends at most
// BatchSize texts per request and gives each request the client's timeout.
// An error never holds the text or the API key.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, 0, len(texts))

	for start := 0; start < len(texts); start += BatchSize {
		end := min(start+BatchSize, len(texts))

		batch, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}

		vectors = append(vectors, batch...)
	}

	return vectors, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if !c.breaker.allow() {
		return nil, ErrCircuitOpen
	}

	vectors, err := c.callBatch(ctx, texts)
	c.breaker.record(err != nil)

	return vectors, err
}

func (c *Client) callBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(request{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("encode the embeddings request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build the embeddings request: %w", err)
	}

	httpRequest.Header.Set("Content-Type", "application/json")

	if c.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResponse, err := c.http.Do(httpRequest)
	if err != nil {
		// The URL error can hold the endpoint, never the texts.
		return nil, fmt.Errorf("call the embeddings endpoint: %w", err)
	}
	defer func() { _ = httpResponse.Body.Close() }()

	if httpResponse.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the embeddings endpoint answered %d", httpResponse.StatusCode)
	}

	var decoded response
	if err := json.NewDecoder(io.LimitReader(httpResponse.Body, maxResponseBytes)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode the embeddings response: %w", err)
	}

	if len(decoded.Data) != len(texts) {
		return nil, fmt.Errorf("the embeddings endpoint returned %d vectors for %d texts", len(decoded.Data), len(texts))
	}

	vectors := make([][]float32, len(texts))

	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(texts) || vectors[item.Index] != nil ||
			len(item.Embedding) == 0 || len(item.Embedding) > maxDimensions {
			return nil, errors.New("the embeddings endpoint returned an invalid vector")
		}

		vectors[item.Index] = item.Embedding
	}

	return vectors, nil
}

// FormatVector returns the text form of a vector that pgvector reads, for
// example `[0.5,1]`. An empty vector gives the empty string.
func FormatVector(vector []float32) string {
	if len(vector) == 0 {
		return ""
	}

	var builder strings.Builder

	builder.WriteByte('[')

	for index, value := range vector {
		if index > 0 {
			builder.WriteByte(',')
		}

		builder.WriteString(strconv.FormatFloat(float64(value), 'g', -1, 32))
	}

	builder.WriteByte(']')

	return builder.String()
}

func valueOrNow(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}

	return now
}
