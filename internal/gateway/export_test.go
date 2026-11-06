package gateway

import (
	"encoding/json"
	"net/http"
	"time"
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
