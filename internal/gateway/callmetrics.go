package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// unknownServer is the server label of a call to a server the catalog
// doesn't offer. The name in such a call is client input, so it never
// becomes a label value.
const unknownServer = "unknown"

type callObservationKey struct{}

// callObservation collects what the metrics need to know about one tool
// call while it runs.
type callObservation struct {
	started time.Time

	mu       sync.Mutex
	server   string
	outcome  string
	duration time.Duration
}

// observationOf returns the observation of the call that ctx belongs to. A
// context with none gets a throwaway one.
func observationOf(ctx context.Context) *callObservation {
	if observation, ok := ctx.Value(callObservationKey{}).(*callObservation); ok {
		return observation
	}

	return &callObservation{}
}

// setOutcome records the outcome of the call unless one is set: the first
// reason a call ends wins.
func (o *callObservation) setOutcome(outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.outcome == "" {
		o.outcome = outcome
	}
}

// forwarded records the result of forwarding a call upstream. outcome is
// one of forward's outcomes.
func (o *callObservation) forwarded(outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if outcome == "failed" || outcome == "too-large" {
		if o.outcome == "" || o.outcome == runtime.CallApproval {
			o.outcome = runtime.CallUpstreamError
		}

		return
	}

	if o.outcome == "" {
		o.outcome = runtime.CallForwarded
	}

	if o.outcome == runtime.CallForwarded && !o.started.IsZero() {
		o.duration = time.Since(o.started)
	}
}

// observeCall counts a finished call. An error return is a call that failed
// inside the gateway before an outcome was known; it counts as refused.
func (m *metaTools) observeCall(observation *callObservation, err error) {
	observation.mu.Lock()
	defer observation.mu.Unlock()

	outcome := observation.outcome
	if outcome == "" {
		if err == nil {
			return
		}

		outcome = runtime.CallRefused
	}

	m.metrics.ObserveCall(observation.server, outcome)

	if observation.duration > 0 {
		m.metrics.ObserveCallDuration(observation.duration)
	}
}

// metricReason is the `reason` label of a policy refusal: `denied` for a
// deny entry and `refused` for any other refusal, whose text is free-form
// and so never a label value.
func metricReason(decision policy.Decision) string {
	if decision.Denied != "" {
		return policy.ReasonDenied
	}

	return "refused"
}
