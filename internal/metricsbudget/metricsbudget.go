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
	"trust_domain": nil,
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
