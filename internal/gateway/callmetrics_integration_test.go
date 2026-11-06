//go:build integration

package gateway_test

import (
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

func TestCallMetrics(t *testing.T) {
	metrics := runtime.NewMetrics()
	fixture := newMCPFixture(t, func(options *gateway.MCPOptions) { options.Metrics = metrics })
	fixture.addEnableTemplate("github", "per-user", anyone)
	fixture.startUpstream(t, "github")

	client := fixture.connectObserved(t, alice())
	fixture.enableWithTools(t, client, "github", "user:alice", "echo", "fails")

	calls := func(server, outcome string) float64 {
		return promtestutil.ToFloat64(metrics.ToolCalls.WithLabelValues(server, outcome))
	}

	// A forwarded call, including one the tool reported as an error.
	require.False(t, callTool(t, client.session, "github__echo", map[string]any{"text": "hi"}).IsError)
	assert.True(t, callTool(t, client.session, "github__fails", nil).IsError)
	assert.InDelta(t, 2, calls("github", runtime.CallForwarded), 0)

	var written dto.Metric
	require.NoError(t, metrics.ToolCallDuration.Write(&written))
	assert.EqualValues(t, 2, written.GetHistogram().GetSampleCount())

	// A tool the server doesn't offer is refused, with no policy step.
	assert.True(t, callTool(t, client.session, "github__missing", nil).IsError)
	assert.InDelta(t, 1, calls("github", runtime.CallRefused), 0)

	// A server the catalog doesn't know never becomes a label value.
	assert.True(t, callTool(t, client.session, "nosuch__tool", nil).IsError)
	assert.InDelta(t, 1, calls("unknown", runtime.CallRefused), 0)

	// A policy refusal raises the refusal counter with the refusing step.
	require.NoError(t, fixture.rules.Set(blockServer("github")))
	assert.True(t, callTool(t, client.session, "github__echo", map[string]any{"text": "hi"}).IsError)
	assert.InDelta(t, 2, calls("github", runtime.CallRefused), 0)
	assert.InDelta(t, 1, promtestutil.ToFloat64(metrics.PolicyRefusals.WithLabelValues("rule", "refused")), 0)

	// Refused and forwarded calls leave the others at zero.
	assert.InDelta(t, 0, calls("github", runtime.CallTimeout), 0)
	assert.InDelta(t, 0, promtestutil.ToFloat64(metrics.CallsWaiting), 0)
}
