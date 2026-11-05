// Package upstreamlimit bounds how much of an upstream MCP server's HTTP
// responses toolgate reads.
//
// The go-sdk's Streamable HTTP client reads an `application/json` response
// whole into memory. Transport stops that read once an operation's byte
// budget is spent, so one upstream answering with a huge body can't exhaust
// the process's memory. Server-sent event streams are left alone: the
// go-sdk bounds each of their events itself.
package upstreamlimit

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync"
)

// Slack is added to a limit on a decoded value (a tool result, a tool
// list) to get the budget for the responses that carry it: it covers the
// JSON-RPC envelope, the `initialize` exchange and encoding differences
// such as whitespace.
const Slack = 1 << 20

// ErrTooLarge is the read error of a response body once its budget is spent.
var ErrTooLarge = errors.New("the upstream response is larger than the limit")

// Budget is the number of response bytes one operation may read, across
// every HTTP response it receives. It is safe for concurrent use.
type Budget struct {
	mu        sync.Mutex
	remaining int64
	exceeded  bool
}

type budgetKey struct{}

// WithBudget returns a context whose requests through Transport may read
// limit response bytes in total, and the budget that counts them.
func WithBudget(ctx context.Context, limit int64) (context.Context, *Budget) {
	budget := &Budget{remaining: limit}

	return context.WithValue(ctx, budgetKey{}, budget), budget
}

// Exceeded reports whether a response tried to read past the budget.
func (b *Budget) Exceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.exceeded
}

// take reserves up to want bytes and returns how many may be read. It
// returns 0 when the budget is already exceeded.
func (b *Budget) take(want int) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.remaining < 0 {
		b.exceeded = true

		return 0
	}

	// One byte past the limit is read so that a body of exactly the
	// limit still ends with io.EOF, and only a longer one is refused.
	allowed := b.remaining + 1
	if int64(want) < allowed {
		return want
	}

	return int(allowed)
}

// spend charges n read bytes and reports whether the budget is exceeded.
func (b *Budget) spend(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.remaining -= int64(n)
	if b.remaining < 0 {
		b.exceeded = true
	}

	return b.exceeded
}

// Transport is an http.RoundTripper that bounds response bodies. A body
// that isn't a server-sent event stream is charged to the request
// context's Budget, or, with no Budget in the context, to a budget of
// Limit bytes of its own.
type Transport struct {
	// Base makes the requests. Nil means http.DefaultTransport.
	Base http.RoundTripper
	// Limit bounds each response body whose request carries no Budget.
	Limit int64
}

// NewClient returns an HTTP client whose responses are bounded by a
// Transport over base.
func NewClient(base *http.Client, limit int64) *http.Client {
	client := *base
	client.Transport = &Transport{Base: base.Transport, Limit: limit}

	return &client
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	response, err := base.RoundTrip(request)
	if err != nil || response.Body == nil || isEventStream(response.Header.Get("Content-Type")) {
		return response, err
	}

	budget, found := request.Context().Value(budgetKey{}).(*Budget)
	if !found {
		budget = &Budget{remaining: t.Limit}
	}

	response.Body = &limitedBody{body: response.Body, budget: budget}

	return response, nil
}

func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)

	return err == nil && mediaType == "text/event-stream"
}

type limitedBody struct {
	body   io.ReadCloser
	budget *Budget
}

func (l *limitedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return l.body.Read(p)
	}

	allowed := l.budget.take(len(p))
	if allowed == 0 {
		return 0, ErrTooLarge
	}

	n, err := l.body.Read(p[:allowed])
	if l.budget.spend(n) {
		return n, ErrTooLarge
	}

	return n, err
}

func (l *limitedBody) Close() error { return l.body.Close() }
