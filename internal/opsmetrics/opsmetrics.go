// Package opsmetrics holds the operational counters that code in several
// packages increments: approval outcomes, MCP requests, sign-in results and
// notification reconnects. Every label has a fixed set of values, so no
// client input becomes a label.
package opsmetrics

import (
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

// Approval request kinds.
const (
	KindTool   = "tool"
	KindServer = "server"
	KindScope  = "scope"
	KindChange = "change"
	KindKey    = "key"
)

// Approval request outcomes.
const (
	OutcomeApproved  = "approved"
	OutcomeDenied    = "denied"
	OutcomeExpired   = "expired"
	OutcomeCancelled = "cancelled"
)

// Sign-in kinds and results.
const (
	AuthUser           = "user"
	AuthServiceAccount = "service_account"
	AuthSPIFFE         = "spiffe"
	AuthConnector      = "connector"

	ResultOK       = "ok"
	ResultRefused  = "refused"
	ResultAttested = "attested"
)

// Upstream session events.
const (
	UpstreamInitialize   = "initialize"
	UpstreamAdopt        = "adopt"
	UpstreamRaceLost     = "race_lost"
	UpstreamNotFound     = "not_found"
	UpstreamDeleteSent   = "delete_sent"
	UpstreamDeleteFailed = "delete_failed"
)

// Other is the label value for anything outside a fixed set.
const Other = "other"

// Kinds is every approval request kind.
var Kinds = []string{KindTool, KindServer, KindScope, KindChange, KindKey}

// Outcomes is every approval request outcome.
var Outcomes = []string{OutcomeApproved, OutcomeDenied, OutcomeExpired, OutcomeCancelled}

// Methods are the MCP methods that have their own label value.
var Methods = []string{"initialize", "tools/list", "tools/call", "subscriptions/listen"}

// Revisions are the MCP protocol revisions that have their own label value.
var Revisions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28"}

// UpstreamEvents is every upstream session event.
var UpstreamEvents = []string{UpstreamInitialize, UpstreamAdopt, UpstreamRaceLost, UpstreamNotFound,
	UpstreamDeleteSent, UpstreamDeleteFailed}

var (
	upstreamSessionEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "toolgate_upstream_session_events_total",
		Help: "Upstream MCP session events, by event.",
	}, []string{"event"})

	approvalRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "toolgate_approval_requests_total",
		Help: "Approval and access requests that reached an outcome, by kind and outcome.",
	}, []string{"kind", "outcome"})

	mcpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "toolgate_mcp_requests_total",
		Help: "MCP requests, by protocol revision and method. Values outside the known sets are other.",
	}, []string{"revision", "method"})

	authResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "toolgate_auth_results_total",
		Help: "Sign-in results, by kind of client and result.",
	}, []string{"kind", "result"})

	notifyReconnects = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "toolgate_notify_reconnects_total",
		Help: "Reconnects of the PostgreSQL LISTEN connection.",
	})
)

// Collectors returns the counters for registration.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{approvalRequests, mcpRequests, authResults, notifyReconnects, upstreamSessionEvents}
}

// ApprovalOutcome counts n requests of kind that reached outcome.
func ApprovalOutcome(kind, outcome string, n int) {
	if n > 0 {
		approvalRequests.WithLabelValues(fold(kind, Kinds), fold(outcome, Outcomes)).Add(float64(n))
	}
}

// MCPRequest counts one MCP request. Revision is the Mcp-Protocol-Version
// header and method the JSON-RPC method; both are client input, so a value
// outside the known sets counts as other.
func MCPRequest(revision, method string) {
	mcpRequests.WithLabelValues(fold(revision, Revisions), fold(method, Methods)).Inc()
}

// AuthResult counts one sign-in result for a kind of client.
func AuthResult(kind, result string) {
	authResults.WithLabelValues(
		fold(kind, []string{AuthUser, AuthServiceAccount, AuthSPIFFE, AuthConnector}),
		fold(result, []string{ResultOK, ResultRefused, ResultAttested})).Inc()
}

// UpstreamSessionEvent counts one upstream session event. An event outside
// UpstreamEvents counts as other.
func UpstreamSessionEvent(event string) {
	upstreamSessionEvents.WithLabelValues(fold(event, UpstreamEvents)).Inc()
}

// NotifyReconnect counts one reconnect of the LISTEN connection.
func NotifyReconnect() { notifyReconnects.Inc() }

func fold(value string, known []string) string {
	if slices.Contains(known, value) {
		return value
	}

	return Other
}

// Value returns the current value of the counter series of name with
// exactly the given labels, or 0 when it has none. Tests read counters with
// it.
func Value(name string, labels map[string]string) float64 {
	registry := prometheus.NewRegistry()
	registry.MustRegister(Collectors()...)

	families, err := registry.Gather()
	if err != nil {
		return 0
	}

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

	series:
		for _, metric := range family.GetMetric() {
			if len(metric.GetLabel()) != len(labels) {
				continue
			}

			for _, pair := range metric.GetLabel() {
				if labels[pair.GetName()] != pair.GetValue() {
					continue series
				}
			}

			return metric.GetCounter().GetValue()
		}
	}

	return 0
}
