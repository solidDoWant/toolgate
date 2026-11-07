// Package resolver is the test resolver: an HTTP
// handler that answers each request with the next value of a counter and an
// expiry the test sets. The e2e tests serve it in the test process; the
// command in cmd/resolver serves it on its own.
package resolver

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request is a request the resolver answered, as the gateway sent it.
type Request struct {
	// Token is the bearer token of the request.
	Token string
	// Body is the request's JSON body.
	Body map[string]any
}

// Resolver answers with a counter: "1" to the first request, "2" to the
// second, and so on. Each value expires Lifetime after the request.
type Resolver struct {
	mu       sync.Mutex
	lifetime time.Duration
	counter  int
	requests []Request
	now      func() time.Time
}

// New returns a resolver whose values expire lifetime after each request.
func New(lifetime time.Duration) *Resolver {
	return &Resolver{lifetime: lifetime, now: time.Now}
}

// SetLifetime changes how long the next values are valid.
func (r *Resolver) SetLifetime(lifetime time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lifetime = lifetime
}

// Requests returns the requests answered so far.
func (r *Resolver) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]Request{}, r.requests...)
}

// ServeHTTP answers a POST with a bearer token and a JSON body with
// {"value", "expiresAt"}; anything else gets 400 or 401.
func (r *Resolver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	token, found := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if !found || token == "" {
		http.Error(writer, "a bearer token is required", http.StatusUnauthorized)

		return
	}

	var body map[string]any
	if request.Method != http.MethodPost ||
		json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&body) != nil {
		http.Error(writer, "POST a JSON object", http.StatusBadRequest)

		return
	}

	r.mu.Lock()
	r.counter++
	value := strconv.Itoa(r.counter)
	expiresAt := r.now().Add(r.lifetime)
	r.requests = append(r.requests, Request{Token: token, Body: body})
	r.mu.Unlock()

	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]string{
		"value":     value,
		"expiresAt": expiresAt.UTC().Format(time.RFC3339),
	})
}
