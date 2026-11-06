package opsmetrics_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/metricsbudget"
	"github.com/solidDoWant/toolgate/internal/opsmetrics"
)

func TestApprovalOutcomeCountsByKindAndOutcome(t *testing.T) {
	labels := func(kind, outcome string) map[string]string {
		return map[string]string{"kind": kind, "outcome": outcome}
	}
	before := opsmetrics.Value("toolgate_approval_requests_total", labels("scope", "cancelled"))

	opsmetrics.ApprovalOutcome("scope", "cancelled", 3)
	opsmetrics.ApprovalOutcome("scope", "cancelled", 0)
	opsmetrics.ApprovalOutcome("bogus", "approved", 1)

	assert.InDelta(t, before+3, opsmetrics.Value("toolgate_approval_requests_total", labels("scope", "cancelled")), 0)
	assert.InDelta(t, 1, opsmetrics.Value("toolgate_approval_requests_total", labels("other", "approved")), 0)
}

// A revision or method from the client outside the known sets is counted as
// other, so client input never makes a label value.
func TestMCPRequestFoldsClientInput(t *testing.T) {
	labels := func(revision, method string) map[string]string {
		return map[string]string{"revision": revision, "method": method}
	}

	opsmetrics.MCPRequest("2025-11-25", "tools/call")
	opsmetrics.MCPRequest("2025-11-25", "tools/call")
	opsmetrics.MCPRequest("2999-01-01", "tools/call")
	opsmetrics.MCPRequest("2025-11-25", "prompts/get")
	opsmetrics.MCPRequest("", "")

	assert.InDelta(t, 2, opsmetrics.Value("toolgate_mcp_requests_total", labels("2025-11-25", "tools/call")), 0)
	assert.InDelta(t, 1, opsmetrics.Value("toolgate_mcp_requests_total", labels("other", "tools/call")), 0)
	assert.InDelta(t, 1, opsmetrics.Value("toolgate_mcp_requests_total", labels("2025-11-25", "other")), 0)
	assert.InDelta(t, 1, opsmetrics.Value("toolgate_mcp_requests_total", labels("other", "other")), 0)
}

func TestNotifyReconnectCounts(t *testing.T) {
	before := opsmetrics.Value("toolgate_notify_reconnects_total", nil)

	opsmetrics.NotifyReconnect()
	opsmetrics.NotifyReconnect()

	assert.InDelta(t, before+2, opsmetrics.Value("toolgate_notify_reconnects_total", nil), 0)
}

func TestPoolCollectorExportsPoolStatistics(t *testing.T) {
	config, err := pgxpool.ParseConfig("postgres://u:p@127.0.0.1:1/db")
	require.NoError(t, err)

	config.MaxConns = 7

	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(opsmetrics.PoolCollector(pool)))

	expected := `
# HELP toolgate_db_pool_connections Connections of the database pool, by state.
# TYPE toolgate_db_pool_connections gauge
toolgate_db_pool_connections{state="idle"} 0
toolgate_db_pool_connections{state="in_use"} 0
toolgate_db_pool_connections{state="max"} 7
`
	require.NoError(t, promtestutil.GatherAndCompare(registry, strings.NewReader(expected), "toolgate_db_pool_connections"))

	families, err := registry.Gather()
	require.NoError(t, err)

	names := make([]string, 0, len(families))
	for _, family := range families {
		names = append(names, family.GetName())
	}

	assert.ElementsMatch(t, []string{"toolgate_db_pool_connections", "toolgate_db_pool_wait_seconds_total", "toolgate_db_pool_acquire_total"}, names)
}

func TestBuildInfoIsOneWithTheVersion(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(opsmetrics.BuildInfo()))

	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	require.Len(t, families[0].GetMetric(), 1)
	assert.Equal(t, "toolgate_build_info", families[0].GetName())
	assert.InDelta(t, 1, families[0].GetMetric()[0].GetGauge().GetValue(), 0)
	assert.Equal(t, "version", families[0].GetMetric()[0].GetLabel()[0].GetName())
}

// Every label combination of the counters stays inside the series budget.
func TestCountersStayInsideSeriesBudget(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(opsmetrics.BuildInfo()))
	registry.MustRegister(opsmetrics.Collectors()...)

	for _, kind := range opsmetrics.Kinds {
		for _, outcome := range opsmetrics.Outcomes {
			opsmetrics.ApprovalOutcome(kind, outcome, 1)
		}
	}

	for _, revision := range append([]string{"x"}, opsmetrics.Revisions...) {
		for _, method := range append([]string{"x"}, opsmetrics.Methods...) {
			opsmetrics.MCPRequest(revision, method)
		}
	}

	for _, kind := range []string{opsmetrics.AuthUser, opsmetrics.AuthServiceAccount, opsmetrics.AuthSPIFFE, opsmetrics.AuthConnector, "x"} {
		for _, result := range []string{opsmetrics.ResultOK, opsmetrics.ResultRefused, opsmetrics.ResultAttested, "x"} {
			opsmetrics.AuthResult(kind, result)
		}
	}

	opsmetrics.NotifyReconnect()

	// At least 16 approval series, 6x5 request series, 5x4 sign-in series,
	// the reconnect counter and the build info. Other tests in the process
	// may have added folded series.
	assert.GreaterOrEqual(t, metricsbudget.Check(t, registry, 0), 16+30+20+1+1)
}
