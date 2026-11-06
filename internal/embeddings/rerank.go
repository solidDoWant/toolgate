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
	"sort"
	"strings"
	"time"
)

// MaxRerankTexts bounds the texts of one rerank request.
const MaxRerankTexts = 100

// Reranker calls a rerank endpoint (`POST <url>/rerank`, the Hugging Face
// text-embeddings-inference route).
type Reranker struct {
	endpoint string
	model    string
	apiKey   string
	timeout  time.Duration
	http     *http.Client
	breaker  breaker
	metrics  *Metrics
}

// NewReranker returns a reranker, or an error when the URL is not an http(s)
// URL or the model is empty. Failures are counted in metrics.
func NewReranker(options Options, metrics *Metrics) (*Reranker, error) {
	parsed, err := url.Parse(options.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("the rerank URL must be an http or https URL")
	}

	if options.Model == "" {
		return nil, errors.New("the rerank model is required")
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	client := options.HTTPClient
	if client == nil {
		client = &http.Client{}
	}

	if metrics == nil {
		metrics = NewMetrics()
	}

	return &Reranker{
		endpoint: strings.TrimRight(options.URL, "/") + "/rerank",
		model:    options.Model, apiKey: options.APIKey, timeout: timeout, http: client,
		breaker: breaker{now: valueOrNow(options.Now)}, metrics: metrics,
	}, nil
}

type rerankRequest struct {
	Query string   `json:"query"`
	Texts []string `json:"texts"`
}

type rerankItem struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

// Rerank returns the indexes of texts, best match for query first. Every
// text appears once. A failure is counted, opens the circuit for
// CircuitOpenFor, and returns an error that never holds the texts.
func (r *Reranker) Rerank(ctx context.Context, query string, texts []string) ([]int, error) {
	if len(texts) == 0 || len(texts) > MaxRerankTexts {
		return nil, fmt.Errorf("a rerank request takes 1 to %d texts", MaxRerankTexts)
	}

	if !r.breaker.allow() {
		r.metrics.count("rerank", "circuit_open")

		return nil, ErrCircuitOpen
	}

	order, err := r.call(ctx, query, texts)
	r.breaker.record(err != nil)

	if err != nil {
		r.metrics.count("rerank", "error")

		return nil, err
	}

	return order, nil
}

func (r *Reranker) call(ctx context.Context, query string, texts []string) ([]int, error) {
	body, err := json.Marshal(rerankRequest{Query: query, Texts: texts})
	if err != nil {
		return nil, fmt.Errorf("encode the rerank request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build the rerank request: %w", err)
	}

	httpRequest.Header.Set("Content-Type", "application/json")

	if r.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	httpResponse, err := r.http.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("call the rerank endpoint: %w", err)
	}
	defer func() { _ = httpResponse.Body.Close() }()

	if httpResponse.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the rerank endpoint answered %d", httpResponse.StatusCode)
	}

	var items []rerankItem
	if err := json.NewDecoder(io.LimitReader(httpResponse.Body, maxResponseBytes)).Decode(&items); err != nil {
		return nil, fmt.Errorf("decode the rerank response: %w", err)
	}

	if len(items) != len(texts) {
		return nil, fmt.Errorf("the rerank endpoint returned %d scores for %d texts", len(items), len(texts))
	}

	seen := make([]bool, len(texts))

	for _, item := range items {
		if item.Index < 0 || item.Index >= len(texts) || seen[item.Index] {
			return nil, errors.New("the rerank endpoint returned an invalid index")
		}

		seen[item.Index] = true
	}

	sortByScore(items)

	order := make([]int, len(items))
	for position, item := range items {
		order[position] = item.Index
	}

	return order, nil
}

// sortByScore sorts items by score, highest first; equal scores keep the
// lower index first.
func sortByScore(items []rerankItem) {
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].Score != items[b].Score {
			return items[a].Score > items[b].Score
		}

		return items[a].Index < items[b].Index
	})
}
