//go:build integration

package runtime_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

func TestApprovalExpiry(t *testing.T) {
	fixture := newStopFixture(t)
	session := fixture.session(t, "approver", "s1", "", "staff")

	// insert adds a pending approval whose expires_at is ago before the
	// database's clock.
	insert := func(t *testing.T, id string, ago time.Duration) {
		t.Helper()

		hash := sha256.Sum256([]byte(id))
		_, err := fixture.pool.Exec(t.Context(), `INSERT INTO approvals (id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, created_at, expires_at)
			VALUES ($1, 'approver', $2, '', 'catalog', 'github', 'merge', $3, now() - interval '1 hour', now() - make_interval(secs => $4))`,
			id, session.ID, hash[:], ago.Seconds())
		require.NoError(t, err)
	}

	status := func(t *testing.T, id string) string {
		t.Helper()

		var value string
		require.NoError(t, fixture.pool.QueryRow(t.Context(), `SELECT status FROM approvals WHERE id = $1`, id).Scan(&value))

		return value
	}

	expires := func(t *testing.T) []string {
		t.Helper()

		rows, err := fixture.pool.Query(t.Context(), `SELECT details->>'approval' FROM audit_events WHERE type = 'approval.expire' ORDER BY id`)
		require.NoError(t, err)

		defer rows.Close()

		var ids []string

		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			ids = append(ids, id)
		}

		return ids
	}

	overdue := "00000000000000000000000000000001"
	recent := "00000000000000000000000000000002"

	insert(t, overdue, time.Minute+5*time.Second)
	insert(t, recent, 30*time.Second)

	expirer := &runtime.ApprovalExpirer{Pool: fixture.pool}
	require.NoError(t, expirer.Run(t.Context()))

	assert.Equal(t, "expired", status(t, overdue), "an approval more than 1 minute past expires_at expires")
	assert.Equal(t, "pending", status(t, recent), "an approval less than 1 minute past expires_at stays pending")
	assert.Equal(t, []string{overdue}, expires(t))

	require.NoError(t, expirer.Run(t.Context()))
	assert.Equal(t, []string{overdue}, expires(t), "an expired approval isn't expired again")
}
