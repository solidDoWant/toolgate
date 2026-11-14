// Package metricsbudget holds the series budget every toolgate metric must
// stay inside, and the check that enforces it.
package metricsbudget

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// BaseSeries is the series allowed per replica whatever the templates.
	BaseSeries = 300
	// SeriesPerTemplate is the series allowed per template per replica.
	SeriesPerTemplate = 10
)

// MaxSeries is the most series one replica may export for the given number
// of templates.
func MaxSeries(templates int) int { return BaseSeries + SeriesPerTemplate*templates }

// AllowedLabels lists the values each label may take. A nil list means the
// values are admin-defined and bounded by other means (template names and
// trust domains). Add a label here, with its fixed values, in the change
// that adds the metric.
var AllowedLabels = map[string][]string{
	"scope":        {"per-session", "per-user", "shared"},
	"status":       {"pending", "starting", "ready", "failed", "stopping"},
	"verified":     {"true", "false"},
	"tool_search":  {"on", "off"},
	"operation":    {"query", "document"},
	"use":          {"query", "document"},
	"trust_domain": nil,
	"server":       nil,
	"version":      nil,
	"kind":         {"tool", "server", "scope", "change", "key", "user", "service_account", "spiffe", "connector", "other", "start", "refresh"},
	// The configured token exchange names, at most 8.
	"exchange": nil,
	"result":   {"ok", "refused", "attested", "other"},
	"method":   {"initialize", "tools/list", "tools/call", "subscriptions/listen", "other"},
	"revision": {"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28", "other"},
	"state":    {"idle", "in_use", "max"},
	"outcome": {
		"forwarded", "refused", "approval", "upstream_error", "timeout", "start_failed",
		"ready", "failed", "approved", "denied", "expired", "cancelled", "other",
		"updated", "missing", "success", "error",
	},
	"step":   {"access", "environment", "blocked", "rule", "temporary-block", "workspace-scope"},
	"reason": {"denied", "refused", "error", "circuit_open"},
	"action": {"replace", "reapply", "gave_up"},
	// The model names are admin-defined, and one is configured at a time.
	"embedding_model": nil,
	"degraded":        {"true", "false"},
	"rank":            {"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20"},
}

// Check gathers the registry and fails the test when it has more series than
// MaxSeries(templates), or a label key or value outside AllowedLabels. It
// returns the series count.
func Check(t *testing.T, registry *prometheus.Registry, templates int) int {
	t.Helper()

	families, err := registry.Gather()
	require.NoError(t, err)

	series := 0

	for _, family := range families {
		for _, metric := range family.GetMetric() {
			series++

			// A histogram exports one series for each bucket, plus the
			// sum, the count and the +Inf bucket.
			if histogram := metric.GetHistogram(); histogram != nil {
				series += len(histogram.GetBucket()) + 2
			}

			for _, pair := range metric.GetLabel() {
				allowed, known := AllowedLabels[pair.GetName()]
				if !assert.Truef(t, known, "%s has label %q outside the allowed set", family.GetName(), pair.GetName()) {
					continue
				}

				if allowed != nil {
					assert.Truef(t, slices.Contains(allowed, pair.GetValue()),
						"%s label %q has value %q outside the allowed set", family.GetName(), pair.GetName(), pair.GetValue())
				}
			}
		}
	}

	assert.LessOrEqual(t, series, MaxSeries(templates), "series count passes the budget")

	return series
}
