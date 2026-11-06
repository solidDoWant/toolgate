package runtime_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/metricsbudget"
	"github.com/solidDoWant/toolgate/internal/opsmetrics"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// Every label combination the operational metrics can take stays inside the
// series budget and the allowed label sets.
func TestMetricsStayInsideSeriesBudget(t *testing.T) {
	const templates = 10

	metrics := runtime.NewMetrics()
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.Collectors()...)

	revisions := []string{runtime.RevisionStateful, runtime.RevisionStateless}

	for _, scope := range []string{"per-session", "per-user", "shared"} {
		for _, status := range []string{"pending", "starting", "ready", "failed", "stopping"} {
			for _, revision := range revisions {
				metrics.Instances.WithLabelValues(scope, status, revision).Set(1)
			}
		}
	}

	metrics.Sessions.WithLabelValues("true").Set(1)
	metrics.Sessions.WithLabelValues("false").Set(1)

	for index := range templates {
		server := fmt.Sprintf("server-%d", index)

		for _, outcome := range []string{
			runtime.CallForwarded, runtime.CallRefused, runtime.CallApproval,
			runtime.CallUpstreamError, runtime.CallTimeout, runtime.CallStartFailed,
		} {
			metrics.ObserveCall(server, outcome)
		}

		metrics.InstanceStarts.WithLabelValues(server, runtime.StartReady).Inc()
		metrics.InstanceStarts.WithLabelValues(server, runtime.StartFailed).Inc()
		metrics.HealthFailures.WithLabelValues(server).Inc()

		for _, action := range []string{runtime.RepairReplace, runtime.RepairReapply, runtime.RepairGaveUp} {
			metrics.Repairs.WithLabelValues(server, action).Inc()
		}
	}

	for _, step := range []string{"access", "environment", "blocked", "rule", "temporary-block", "workspace-scope"} {
		for _, reason := range []string{"denied", "refused"} {
			metrics.ObservePolicyRefusal(step, reason)
		}
	}

	metrics.ObserveCallDuration(time.Second)
	metrics.WaitingForStart(1)

	for _, kind := range opsmetrics.Kinds {
		metrics.ApprovalsPending.WithLabelValues(kind).Set(1)
	}

	metrics.Leader.Set(1)

	// Instances 30, stuck 1, sessions 2, calls 60, call duration 1, policy
	// 12, starts 20, start duration 1, waiting 1, health 10, repairs 30,
	// pending 4, leader 1.
	assert.Equal(t, 30+1+2+60+1+12+20+1+1+10+30+4+1, metricsbudget.Check(t, registry, templates))
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
