package runtime_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/embeddings"
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

	for _, search := range []string{"on", "off"} {
		for _, verified := range []string{"true", "false"} {
			metrics.Sessions.WithLabelValues(search, verified).Set(1)
		}
	}

	embedMetrics := embeddings.NewMetrics()
	registry.MustRegister(embedMetrics.Collectors()...)

	for _, operation := range []string{embeddings.OperationQuery, embeddings.OperationDocument} {
		embedMetrics.Duration(operation).Observe(1)

		for _, reason := range []string{"error", "circuit_open"} {
			embedMetrics.Counter(operation, reason).Inc()
		}
	}

	for index := range templates {
		server := fmt.Sprintf("server-%d", index)

		for _, outcome := range []string{
			runtime.CallForwarded, runtime.CallRefused, runtime.CallApproval,
			runtime.CallUpstreamError, runtime.CallTimeout, runtime.CallStartFailed, runtime.CallCached,
		} {
			metrics.ObserveCall(server, outcome)
		}

		for _, result := range []string{runtime.CacheHit, runtime.CacheMiss} {
			metrics.ObserveCacheLookup(server, result)
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

	for _, result := range []string{"stored", "entry_too_large", "template_full", "session_ended", runtime.CacheStoreFailed} {
		metrics.ObserveCacheStore(result)
	}

	metrics.ObserveCallDuration(time.Second)
	metrics.WaitingForStart(1)

	for _, kind := range opsmetrics.Kinds {
		metrics.ApprovalsPending.WithLabelValues(kind).Set(1)
	}

	metrics.Leader.Set(1)

	for _, scope := range []string{"per-template", "per-user"} {
		metrics.PersistentVolumeBytes.WithLabelValues(scope).Set(1)

		for _, state := range []string{"in_use", "idle", "deleting"} {
			metrics.PersistentVolumes.WithLabelValues(scope, state).Set(1)
		}
	}

	for _, event := range runtime.VolumeEvents {
		metrics.ObserveVolumeEvent(event)
	}

	exchangeMetrics := credentials.NewMetrics()
	registry.MustRegister(exchangeMetrics.Collectors()...)

	for index := range 8 {
		for _, kind := range []string{credentials.KindStart, credentials.KindRefresh} {
			for _, outcome := range []string{"success", "refused", "error"} {
				exchangeMetrics.Exchanges.WithLabelValues(fmt.Sprintf("exchange-%d", index), kind, outcome).Inc()
			}
		}
	}

	// Instances 30, stuck 1, sessions 4, calls 70, cache lookups 20, cache
	// stores 5, call duration 8 buckets + 3, policy 12, starts 20, start duration 7 buckets + 3, waiting 1,
	// health 10, repairs 30, embedding errors 4, embedding duration 2 ×
	// (10 buckets + 3), pending 5, leader 1, token exchanges 8 × 2 × 3, volumes 6 + 2 + 7.
	assert.Equal(t, 30+1+4+70+20+5+11+12+20+10+1+10+30+4+26+5+1+48+6+2+7, metricsbudget.Check(t, registry, templates))
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
