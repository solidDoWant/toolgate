//go:build integration

package runtime_test

import (
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionsGaugeCountsOpenSessions(t *testing.T) {
	fixture := newStopFixture(t)

	sessions := func(verified string) float64 {
		return promtestutil.ToFloat64(fixture.metrics.Sessions.WithLabelValues(verified))
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
