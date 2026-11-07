//go:build integration

package gateway_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const checkReason = "GET http://db-proxy:8080/healthz answered 503, expected 200"

// setHealth makes the reconciler's health write on instance id.
func (f *mcpFixture) setHealth(t *testing.T, id string, healthy bool, reason string) {
	t.Helper()

	instance, err := db.New(f.pool).GetInstance(t.Context(), id)
	require.NoError(t, err)

	changed, err := runtime.PostgresStartStore{Pool: f.pool}.SetHealth(t.Context(), instance, healthy, reason)
	require.NoError(t, err)
	require.True(t, changed)
}

func TestUnhealthyInstance(t *testing.T) {
	t.Run("an instance that becomes unhealthy lets the call in flight finish, refuses later calls with not-ready and withdraws the tools", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		upstream := fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		instance := fixture.enableWithTools(t, client, "github", "user:alice", "slow")
		require.Eventually(t, func() bool { return client.listChangedCount() == 1 }, waitTimeout, 10*time.Millisecond)

		calls := make(chan *mcp.CallToolResult, 1)

		go func() {
			result, err := client.session.CallTool(t.Context(), &mcp.CallToolParams{Name: "github__slow"})
			if err != nil {
				result = &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
			}

			calls <- result
		}()

		select {
		case <-upstream.started:
		case <-time.After(waitTimeout):
			require.FailNow(t, "the call never reached the upstream server")
		}

		fixture.setHealth(t, instance.ID, false, checkReason)

		require.Eventually(t, func() bool { return client.listChangedCount() == 2 }, waitTimeout, 10*time.Millisecond)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session), "the unhealthy instance's tools are withdrawn")

		close(upstream.release)

		select {
		case result := <-calls:
			require.False(t, result.IsError, resultText(result))
			assert.Equal(t, "slow done", resultText(result))
		case <-time.After(waitTimeout):
			require.FailNow(t, "the in-flight call did not finish")
		}

		later := callTool(t, client.session, "github__slow", nil)
		assert.True(t, later.IsError)
		assert.Equal(t, "`github` is unhealthy: "+checkReason+". toolgate is repairing it.", resultText(later))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 2)
		assert.Equal(t, "allow", decisions[0].Details["decision"])
		assert.Equal(t, "refuse", decisions[1].Details["decision"])
		assert.Equal(t, "not-ready", decisions[1].Details["reason"])
		assert.Equal(t, checkReason, decisions[1].Details["detail"])
		assert.Len(t, upstream.started, 0, "the later call reached the upstream server")

		status := serverStatus(t, client.session, "github")
		assert.Equal(t, "ready", status.Status)
		assert.Equal(t, "unhealthy: "+checkReason, status.Reason)

		health := fixture.auditRows(t, "instance.health")
		require.Len(t, health, 1)
		assert.Equal(t, "github", health[0].Server)
		assert.Equal(t, false, health[0].Details["healthy"])
		assert.Equal(t, checkReason, health[0].Details["reason"])
		assert.Equal(t, instance.ID, health[0].Details["instance"])

		// Healthy again: the tools come back with list_changed.
		fixture.setHealth(t, instance.ID, true, "")

		require.Eventually(t, func() bool { return client.listChangedCount() == 3 }, waitTimeout, 10*time.Millisecond)
		assert.ElementsMatch(t, append(metaToolNames, "github__slow"), toolNames(t, client.session))
		assert.Empty(t, serverStatus(t, client.session, "github").Reason)
		assert.Len(t, fixture.auditRows(t, "instance.health"), 2)
	})

	t.Run("an enable whose instance is unhealthy returns the reason and leaves the server enabled; the tools arrive once it is healthy", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())

		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, fixture.catalogTemplate(t, "github"), "user:alice")

		// The instance becomes ready and unhealthy before the waiting
		// enable reads it.
		tools, err := json.Marshal([]*mcp.Tool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}})
		require.NoError(t, err)

		require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
			queries := db.New(tx)

			if _, err := queries.SetInstanceStarting(t.Context(), db.SetInstanceStartingParams{ID: instance.ID, Generation: instance.Generation}); err != nil {
				return err
			}

			if _, err := queries.SetInstanceReady(t.Context(), db.SetInstanceReadyParams{ID: instance.ID, Tools: tools}); err != nil {
				return err
			}

			if _, err := queries.SetInstanceUnhealthy(t.Context(), db.SetInstanceUnhealthyParams{ID: instance.ID, Reason: checkReason}); err != nil {
				return err
			}

			return store.Notify(t.Context(), tx, store.KindInstance, instance.ID)
		}))

		enabled := awaitEnable(t, results)
		assert.Equal(t, "`github` is unhealthy: "+checkReason+". toolgate is repairing it.", enabled.err)

		status := serverStatus(t, client.session, "github")
		assert.True(t, status.Enabled, "the server stays enabled")
		assert.Equal(t, "unhealthy: "+checkReason, status.Reason)
		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))

		before := client.listChangedCount()

		fixture.setHealth(t, instance.ID, true, "")

		require.Eventually(t, func() bool { return client.listChangedCount() > before }, waitTimeout, 10*time.Millisecond)
		assert.ElementsMatch(t, append(metaToolNames, "github__echo"), toolNames(t, client.session))
	})
}
