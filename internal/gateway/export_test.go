package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// StatelessBindings returns how many stateless bindings the MCP handler
// holds.
func StatelessBindings(handler http.Handler) int {
	endpoint := handler.(*mcpEndpoint)

	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()

	return len(endpoint.stateless)
}

// CanonicalArguments exposes canonicalArguments to tests.
func CanonicalArguments(arguments json.RawMessage) (json.RawMessage, []byte, error) {
	return canonicalArguments(arguments)
}

// DurationWords exposes durationWords to tests.
var DurationWords = durationWords

// SetBeforeApprovalUse makes hook run before each use of an approval, and
// returns a function that removes it.
func SetBeforeApprovalUse(hook func()) func() {
	beforeApprovalUse = hook

	return func() { beforeApprovalUse = nil }
}

// SetApprovalWaitInterval sets how often a waiting call re-reads its
// approval, and returns a function that restores it.
func SetApprovalWaitInterval(interval time.Duration) func() {
	previous := approvalWaitInterval
	approvalWaitInterval = interval

	return func() { approvalWaitInterval = previous }
}

// ApprovalWaiters exposes the per-principal count of waiting calls.
type ApprovalWaiters struct{ waiters approvalWaiters }

// Acquire takes a waiting slot for principal.
func (w *ApprovalWaiters) Acquire(principal string) bool { return w.waiters.acquire(principal) }

// Release frees a waiting slot of principal.
func (w *ApprovalWaiters) Release(principal string) { w.waiters.release(principal) }

// WaitingCalls returns how many calls of principal wait for an approval on
// the MCP handler.
func WaitingCalls(handler http.Handler, principal string) int {
	return handler.(*mcpEndpoint).waiters.count(principal)
}

// WrapEndMCPSession wraps how the endpoint ends an MCP session at a
// client's DELETE, so a test can hold it after the end transaction commits.
func WrapEndMCPSession(handler http.Handler, wrap func(next func(ctx context.Context, id string) error) func(ctx context.Context, id string) error) {
	endpoint := handler.(*mcpEndpoint)
	endpoint.endMCPSession = wrap(endpoint.endMCPSession)
}

// HeldMCPSessions returns how many MCP session copies the MCP handler
// holds.
func HeldMCPSessions(handler http.Handler) int {
	endpoint := handler.(*mcpEndpoint)

	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()

	return len(endpoint.bindings)
}

// ResolveSession runs the endpoint's session lookup for identity, as a
// request that passed the MCP session binding check does.
func ResolveSession(ctx context.Context, handler http.Handler, identity auth.Identity) (db.Session, error) {
	return handler.(*mcpEndpoint).sessions.resolve(ctx, identity, false)
}
