//go:build integration

package gateway_test

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store"
)

// policyChangeBound is how soon after a grant or temporary block changes or
// expires open sessions must get list_changed (docs/design/phase-2.md §3).
const policyChangeBound = 5 * time.Second

var rowCounter atomic.Int64

// rowID returns a new 32-hex-character ID for a grant or block row.
func rowID() string {
	return fmt.Sprintf("%032x", rowCounter.Add(1))
}

var optInForAlice = v1alpha1.Access{Users: []string{"alice"}, Environments: "opt-in"}

// insertGrant inserts a live grant for principal on server, as the Grants
// page does: the row and grant:<id> in one transaction. It returns the ID and
// the expiry PostgreSQL recorded.
func (f *mcpFixture) insertGrant(t *testing.T, principal, server, tool, session string, lifetime time.Duration) (string, time.Time) {
	t.Helper()

	id := rowID()

	var (
		sessionID *string
		expiresAt time.Time
	)

	if session != "" {
		sessionID = &session
	}

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(t.Context(), `INSERT INTO grants (id, principal, template_ns, template_name, tool, session_id, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7)) RETURNING expires_at`,
			id, principal, templateNamespace, server, tool, sessionID, lifetime.Seconds()).Scan(&expiresAt); err != nil {
			return err
		}

		return store.Notify(t.Context(), tx, store.KindGrant, id)
	}))

	return id, expiresAt
}

// revokeGrant revokes a grant and sends grant:<id>.
func (f *mcpFixture) revokeGrant(t *testing.T, id string) {
	t.Helper()

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `UPDATE grants SET revoked_at = now(), revoked_by = 'test' WHERE id = $1`, id); err != nil {
			return err
		}

		return store.Notify(t.Context(), tx, store.KindGrant, id)
	}))
}

// insertBlock inserts a temporary block. With notify, block:<id> is sent in
// the same transaction, as the Blocks page does.
func (f *mcpFixture) insertBlock(t *testing.T, match v1alpha1.RuleMatch, reason string, lifetime time.Duration, notify bool) (string, time.Time) {
	t.Helper()

	id := rowID()
	encoded, err := json.Marshal(match)
	require.NoError(t, err)

	var expiresAt time.Time

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(t.Context(), `INSERT INTO temporary_blocks (id, match, reason, created_by, expires_at)
			VALUES ($1, $2, $3, 'admin', now() + make_interval(secs => $4)) RETURNING expires_at`,
			id, encoded, reason, lifetime.Seconds()).Scan(&expiresAt); err != nil {
			return err
		}

		if !notify {
			return nil
		}

		return store.Notify(t.Context(), tx, store.KindBlock, id)
	}))

	return id, expiresAt
}

func TestGrants(t *testing.T) {
	t.Run("row 35: a grant created on another replica lets the retry of a refused enable through, and the call records the grant", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", optInForAlice)
		fixture.startUpstream(t, "github")
		client := fixture.connectObserved(t, alice())

		refused := awaitEnable(t, startEnable(t, client.session, "github"))
		assert.Contains(t, refused.err, `Server "github" can be used only from an attested environment`)

		// Another replica writes the grant: the replicas share only rows.
		grant, _ := fixture.insertGrant(t, "alice", "github", "", "", time.Hour)

		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "granted"})
		require.False(t, result.IsError, resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "allow", decisions[0].Details["decision"])
		assert.Equal(t, grant, decisions[0].Details["grant"])
	})

	t.Run("row 37: a grant reaching expires_at refuses the next call and sends list_changed within 5 s", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", optInForAlice)
		fixture.startUpstream(t, "github")

		_, expiresAt := fixture.insertGrant(t, "alice", "github", "", "", 3*time.Second)

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, time.Until(expiresAt)+policyChangeBound, 10*time.Millisecond,
			"no list_changed within 5 s of the grant's expiry")
		assert.Less(t, time.Since(expiresAt), policyChangeBound)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		refused := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		assert.True(t, refused.IsError)
		assert.Contains(t, resultText(refused), "attested environment")
		assert.Equal(t, []string{"github"}, fixture.enabledServers(t, "alice", "session-1"), "a grant's expiry stops nothing")
	})

	t.Run("row 36: a revoked grant refuses the next call and the principal's streams get list_changed within 5 s; other principals get none", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", optInForAlice)
		fixture.addEnableTemplate("open", "per-user", anyone)
		fixture.startUpstream(t, "github")
		fixture.startUpstream(t, "open")

		grant, _ := fixture.insertGrant(t, "alice", "github", "", "", time.Hour)

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		bobs := fixture.connectObserved(t, bob())
		fixture.enableWithTools(t, bobs, "open", "user:bob", "echo")
		require.Eventually(t, func() bool { return bobs.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		revoked := time.Now()

		fixture.revokeGrant(t, grant)

		refused := callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		assert.True(t, refused.IsError, "the next decision loads fresh state")

		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, policyChangeBound, 10*time.Millisecond)
		assert.Less(t, time.Since(revoked), policyChangeBound)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))
		assert.Equal(t, 1, bobs.listChangedCount())
	})

	t.Run("row 38: a grant revoked while the enable waits for its instance refuses the enable when the instance is ready", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", optInForAlice)

		grant, _ := fixture.insertGrant(t, "alice", "github", "", "", time.Hour)

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, "user:alice")

		fixture.revokeGrant(t, grant)
		fixture.markReady(t, instance.ID, "echo")

		assert.Contains(t, awaitEnable(t, results).err, "attested environment")
		assert.Empty(t, fixture.enabledServers(t, "alice", "session-1"), "the refused enable deletes its session_servers row")
	})

	t.Run("row 39: a grant narrowed to one session refuses an enable from another session of the same user", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", optInForAlice)
		fixture.insertGrant(t, "alice", "github", "", "session-1", time.Hour)

		otherSession := alice()
		otherSession.session = "session-2"
		other := fixture.connectObserved(t, otherSession)
		assert.Contains(t, awaitEnable(t, startEnable(t, other.session, "github")).err, "attested environment")

		narrowed := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, narrowed, "github", "user:alice", "echo")
	})
}

func TestTemporaryBlocks(t *testing.T) {
	t.Run("row 50: a temporary block made on one replica sends list_changed on every replica within 5 s and refuses calls with its reason", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		first := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, first, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return first.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		other := fixture.replica(t)
		second := fixture.connectObservedTo(t, other.URL, bob())
		results := startEnable(t, second.session, "github")
		bobInstance := fixture.awaitInstance(t, fixture.catalogTemplate(t, "github"), "user:bob")
		fixture.markReady(t, bobInstance.ID, "echo")
		require.Empty(t, awaitEnable(t, results).err)
		require.Eventually(t, func() bool { return second.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		created := time.Now()
		block, expiresAt := fixture.insertBlock(t, v1alpha1.RuleMatch{Servers: []string{"github"}}, "Incident 7.", time.Hour, true)

		require.Eventually(t, func() bool { return first.listChangedCount() == 2 && second.listChangedCount() == 2 }, policyChangeBound, 10*time.Millisecond,
			"not every replica sent list_changed within 5 s")
		assert.Less(t, time.Since(created), policyChangeBound)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, first.session))
		assert.ElementsMatch(t, metaToolNames, toolNames(t, second.session))

		refused := callTool(t, first.session, "github__echo", map[string]any{"text": "x"})
		assert.True(t, refused.IsError)
		assert.Equal(t, fmt.Sprintf("Blocked by an admin until %s: Incident 7. (block %s).", expiresAt.UTC().Format(time.RFC3339), block), resultText(refused))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, block, decisions[0].Details["block"])
		assert.Equal(t, "temporary-block", decisions[0].Details["step"])
	})

	t.Run("row 51: a temporary block reaching expires_at sends list_changed and the tools come back without a new enable", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		_, expiresAt := fixture.insertBlock(t, v1alpha1.RuleMatch{Tools: []string{"github__echo"}}, "Short freeze.", 3*time.Second, true)
		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, policyChangeBound, 10*time.Millisecond)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		require.Eventually(t, func() bool { return client.listChangedCount() == 3 }, time.Until(expiresAt)+policyChangeBound, 10*time.Millisecond,
			"no list_changed within 5 s of the block's expiry")
		assert.Less(t, time.Since(expiresAt), policyChangeBound)
		assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, client.session))

		result := callTool(t, client.session, "github__echo", map[string]any{"text": "back"})
		require.False(t, result.IsError, resultText(result))
	})

	t.Run("row 52: after a LISTEN reconnect every stream gets list_changed and the expiry timer is reset", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		// The block's notification is lost: only the reconnect tells the
		// replica about it.
		_, expiresAt := fixture.insertBlock(t, v1alpha1.RuleMatch{Servers: []string{"github"}}, "Lost notification.", 4*time.Second, false)

		var terminated int
		require.NoError(t, fixture.pool.QueryRow(t.Context(),
			"SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'LISTEN%' AND pid <> pg_backend_pid()) AS t").Scan(&terminated))
		require.Positive(t, terminated)

		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, policyChangeBound, 10*time.Millisecond,
			"no list_changed after the LISTEN reconnect")
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		require.Eventually(t, func() bool { return client.listChangedCount() == 3 }, time.Until(expiresAt)+policyChangeBound, 10*time.Millisecond,
			"the expiry timer wasn't reset after the reconnect")
		assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, client.session))
	})
}
