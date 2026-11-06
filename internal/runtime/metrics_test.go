package runtime_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/metricsbudget"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// Every label combination the instance metrics can take stays inside the
// series budget and the allowed label sets.
func TestMetricsStayInsideSeriesBudget(t *testing.T) {
	const templates = 10

	metrics := runtime.NewMetrics()
	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(metrics.Instances))
	require.NoError(t, registry.Register(metrics.NamespacesStuck))
	require.NoError(t, registry.Register(metrics.Sessions))

	for _, scope := range []string{"per-session", "per-user", "shared"} {
		for _, status := range []string{"pending", "starting", "ready", "failed", "stopping"} {
			metrics.Instances.WithLabelValues(scope, status).Set(1)
		}
	}

	metrics.Sessions.WithLabelValues("true").Set(1)
	metrics.Sessions.WithLabelValues("false").Set(1)

	assert.Equal(t, 15+1+2, metricsbudget.Check(t, registry, templates))
}

// A label outside the allowed sets fails the check.
func TestSeriesBudgetRefusesUnknownLabel(t *testing.T) {
	registry := prometheus.NewRegistry()
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "toolgate_x"}, []string{"user"})
	require.NoError(t, registry.Register(gauge))
	gauge.WithLabelValues("alice").Set(1)

	inner := &testing.T{}
	done := make(chan struct{})

	go func() {
		defer close(done)

		metricsbudget.Check(inner, registry, 10)
	}()

	<-done

	assert.True(t, inner.Failed())
}
