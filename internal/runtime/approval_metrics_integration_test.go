//go:build integration

package runtime_test

import (
	"crypto/sha256"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// The pending gauge counts unexpired pending requests by kind, the leader
// gauge is 1 after a pass, and an expired request counts once by its kind.
func TestApprovalMetricsFollowRequests(t *testing.T) {
	fixture := newStopFixture(t)
	session := fixture.session(t, "approver", "s1", "", "staff")

	outcome := func(kind, outcome string) float64 {
		return opsmetrics.Value("toolgate_approval_requests_total", map[string]string{"kind": kind, "outcome": outcome})
	}
	pending := func(kind string) float64 {
		return promtestutil.ToFloat64(fixture.metrics.ApprovalsPending.WithLabelValues(kind))
	}

	toolApproval := func(id string, expiresIn time.Duration) {
		hash := sha256.Sum256([]byte(id))
		_, err := fixture.pool.Exec(t.Context(), `INSERT INTO approvals (id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, created_at, expires_at)
			VALUES ($1, 'approver', $2, '', 'catalog', 'github', 'merge', $3, now() - interval '2 hours', now() + make_interval(secs => $4))`,
			id, session.ID, hash[:], expiresIn.Seconds())
		require.NoError(t, err)
	}

	accessRequest := func(id, kind string, expiresIn time.Duration) {
		_, err := fixture.pool.Exec(t.Context(), `INSERT INTO access_requests (id, principal, kind, workspace, verified, change, created_at, expires_at)
			VALUES ($1, 'approver', $2, '', false, '{}'::jsonb, now() - interval '2 hours', now() + make_interval(secs => $3))`,
			id, kind, expiresIn.Seconds())
		require.NoError(t, err)
	}

	fixture.pass(t)
	assert.InDelta(t, 1, promtestutil.ToFloat64(fixture.metrics.Leader), 0)

	for _, kind := range opsmetrics.Kinds {
		assert.InDelta(t, 0, pending(kind), 0, kind)
	}

	toolApproval("00000000000000000000000000000001", time.Hour)
	toolApproval("00000000000000000000000000000002", time.Hour)
	// Past expiry by more than the grace: the next pass expires it.
	toolApproval("00000000000000000000000000000003", -time.Hour)
	accessRequest("00000000000000000000000000000004", "change", time.Hour)
	accessRequest("00000000000000000000000000000005", "scope", -time.Hour)

	toolExpired, scopeExpired := outcome("tool", "expired"), outcome("scope", "expired")

	expirer := &runtime.ApprovalExpirer{Pool: fixture.pool}
	require.NoError(t, expirer.Run(t.Context()))
	fixture.pass(t)

	assert.InDelta(t, 2, pending("tool"), 0)
	assert.InDelta(t, 1, pending("change"), 0)
	assert.InDelta(t, 0, pending("scope"), 0)
	assert.InDelta(t, 0, pending("server"), 0)
	assert.InDelta(t, toolExpired+1, outcome("tool", "expired"), 0)
	assert.InDelta(t, scopeExpired+1, outcome("scope", "expired"), 0)

	// A second run expires nothing more.
	require.NoError(t, expirer.Run(t.Context()))
	assert.InDelta(t, toolExpired+1, outcome("tool", "expired"), 0)
}
