//go:build integration

package runtime_test

import (
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

func TestSessionsGaugeCountsOpenSessions(t *testing.T) {
	fixture := newStopFixture(t)

	// sessions sums over the tool search label.
	sessions := func(verified string) float64 {
		return promtestutil.ToFloat64(fixture.metrics.Sessions.WithLabelValues("on", verified)) +
			promtestutil.ToFloat64(fixture.metrics.Sessions.WithLabelValues("off", verified))
	}

	fixture.pass(t)
	assert.InDelta(t, 0, sessions("true"), 0)
	assert.InDelta(t, 0, sessions("false"), 0)

	unverified := fixture.session(t, "open-user", "s1", "", "staff")
	fixture.sessionOf(t, "open-user", "s2", "ws/a", "user", "user", "")
	attested := fixture.sessionOf(t, "attested-user", "s3", "ws/a", "attested", "user", "")

	fixture.pass(t)
	assert.InDelta(t, 1, sessions("true"), 0)
	assert.InDelta(t, 2, sessions("false"), 0)

	_, err := fixture.queries.EndLiveSession(t.Context(), unverified.ID)
	require.NoError(t, err)
	_, err = fixture.queries.EndLiveSession(t.Context(), attested.ID)
	require.NoError(t, err)

	fixture.pass(t)
	assert.InDelta(t, 0, sessions("true"), 0)
	assert.InDelta(t, 1, sessions("false"), 0)
}

func TestSessionsGaugeSplitsByToolSearch(t *testing.T) {
	fixture := newStopFixture(t)

	for _, row := range []struct {
		session string
		search  bool
	}{{"on-1", true}, {"on-2", true}, {"off-1", false}} {
		_, err := fixture.queries.UpsertSession(t.Context(), db.UpsertSessionParams{
			Principal: "user", SessionID: row.session, Workspace: "claimed/default",
			Attestation: "user", Kind: "user", Groups: []string{}, ToolSearch: row.search,
		})
		require.NoError(t, err)
	}

	fixture.pass(t)

	gauge := func(search, verified string) float64 {
		return promtestutil.ToFloat64(fixture.metrics.Sessions.WithLabelValues(search, verified))
	}

	assert.InDelta(t, 2, gauge("on", "false"), 0)
	assert.InDelta(t, 1, gauge("off", "false"), 0)
	assert.InDelta(t, 0, gauge("on", "true"), 0)
	assert.InDelta(t, 0, gauge("off", "true"), 0)
}
