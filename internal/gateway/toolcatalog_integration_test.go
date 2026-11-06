//go:build integration

package gateway_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	toolruntime "github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

func (f *mcpFixture) learn(t *testing.T, uid string, generation int64, tools ...string) {
	t.Helper()

	listed := make([]*mcp.Tool, 0, len(tools))
	for _, name := range tools {
		listed = append(listed, &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}})
	}

	encoded, err := json.Marshal(listed)
	require.NoError(t, err)

	require.NoError(t, db.New(f.pool).UpsertToolCatalog(t.Context(), db.UpsertToolCatalogParams{
		TemplateUid: uid, Generation: generation, TemplateNs: templateNamespace, TemplateName: "github",
		Tools: encoded, InstanceID: "learned",
	}))
}

// becomeReady is what the reconciler does when an instance becomes ready.
func (f *mcpFixture) becomeReady(t *testing.T, store toolruntime.PostgresStartStore, id string, tools ...string) {
	t.Helper()

	instance, err := db.New(f.pool).GetInstance(t.Context(), id)
	require.NoError(t, err)

	changed, err := store.MarkStarting(t.Context(), id, instance.Generation, nil, time.Time{})
	require.NoError(t, err)
	require.True(t, changed)

	listed := make([]*mcp.Tool, 0, len(tools))
	for _, name := range tools {
		listed = append(listed, &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}})
	}

	encoded, err := json.Marshal(listed)
	require.NoError(t, err)

	changed, err = store.MarkReady(t.Context(), instance, encoded, toolruntime.RevisionStateful)
	require.NoError(t, err)
	require.True(t, changed)
}

func (f *mcpFixture) catalogRows(t *testing.T, uid string) []db.ToolCatalog {
	t.Helper()

	rows, err := db.New(f.pool).ListToolCatalogForTemplate(t.Context(), uid)
	require.NoError(t, err)

	return rows
}

func TestToolCatalogLists(t *testing.T) {
	t.Run("a starting instance lists the catalog list; row 185: a different real list reaches the session and other scope keys", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.learn(t, string(template.UID), template.Generation, "old")
		start := toolruntime.PostgresStartStore{Pool: fixture.pool}

		aliceClient := fixture.connectObserved(t, alice())
		bobClient := fixture.connectObserved(t, bob())

		aliceResults := startEnable(t, aliceClient.session, "github")
		bobResults := startEnable(t, bobClient.session, "github")
		aliceInstance := fixture.awaitInstance(t, template, "user:alice")
		fixture.awaitInstance(t, template, "user:bob")

		assert.ElementsMatch(t, append(metaToolNames, "github__old"), toolNames(t, aliceClient.session), "pending: the catalog list")

		fixture.becomeReady(t, start, aliceInstance.ID, "new")

		require.Empty(t, awaitEnable(t, aliceResults).err)
		assert.ElementsMatch(t, append(metaToolNames, "github__new"), toolNames(t, aliceClient.session), "ready: the real list")
		assert.ElementsMatch(t, append(metaToolNames, "github__new"), toolNames(t, bobClient.session), "another scope key: the new catalog row")

		rows := fixture.catalogRows(t, string(template.UID))
		require.Len(t, rows, 1)
		assert.Equal(t, template.Generation, rows[0].Generation)
		assert.Equal(t, aliceInstance.ID, rows[0].InstanceID)
		assert.JSONEq(t, `[{"name":"new","inputSchema":{"type":"object"}}]`, string(rows[0].Tools))

		_ = bobResults
	})

	t.Run("row 187: with no row for the current generation the list falls back to the newest older row", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.learn(t, string(template.UID), template.Generation-1, "older")
		fixture.learn(t, string(template.UID), template.Generation+1, "from-the-future")

		client := fixture.connectObserved(t, alice())
		startEnable(t, client.session, "github")
		fixture.awaitInstance(t, template, "user:alice")

		assert.ElementsMatch(t, append(metaToolNames, "github__older"), toolNames(t, client.session))
	})

	t.Run("row 188: declared tools are listed while others start and nothing is learned from a ready instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		template.Spec.DeclaredTools = []v1alpha1.DeclaredTool{{Name: "declared", InputSchema: &runtime.RawExtension{Raw: []byte(`{"type":"object"}`)}}}
		fixture.catalog.Set(template, nil)
		fixture.learn(t, string(template.UID), template.Generation, "learned")

		start := toolruntime.PostgresStartStore{Pool: fixture.pool, DeclaresTools: func(db.Instance) bool { return true }}

		aliceClient := fixture.connectObserved(t, alice())
		bobClient := fixture.connectObserved(t, bob())

		aliceResults := startEnable(t, aliceClient.session, "github")
		startEnable(t, bobClient.session, "github")
		aliceInstance := fixture.awaitInstance(t, template, "user:alice")
		fixture.awaitInstance(t, template, "user:bob")

		assert.ElementsMatch(t, append(metaToolNames, "github__declared"), toolNames(t, aliceClient.session))

		fixture.becomeReady(t, start, aliceInstance.ID, "real")
		require.Empty(t, awaitEnable(t, aliceResults).err)

		assert.ElementsMatch(t, append(metaToolNames, "github__real"), toolNames(t, aliceClient.session))
		assert.ElementsMatch(t, append(metaToolNames, "github__declared"), toolNames(t, bobClient.session))

		rows := fixture.catalogRows(t, string(template.UID))
		require.Len(t, rows, 1)
		assert.Equal(t, "learned", rows[0].InstanceID, "the row is untouched")
	})

	t.Run("a failed instance of the current generation lists no tools; of an older generation it lists the catalog", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			generation int64
			want       []string
		}{
			{"current generation", 2, metaToolNames},
			{"older generation", 1, append(append([]string{}, metaToolNames...), "github__old")},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newMCPFixture(t)
				template := fixture.addEnableTemplate("github", "per-user", anyone)
				require.Equal(t, int64(2), template.Generation)
				fixture.learn(t, string(template.UID), template.Generation, "old")
				start := toolruntime.PostgresStartStore{Pool: fixture.pool}

				client := fixture.connectObserved(t, alice())
				results := startEnable(t, client.session, "github")
				instance := fixture.awaitInstance(t, template, "user:alice")

				changed, err := start.MarkStarting(t.Context(), instance.ID, test.generation, nil, time.Time{})
				require.NoError(t, err)
				require.True(t, changed)

				changed, err = start.MarkFailed(t.Context(), instance.ID, "starting", "image pull failed")
				require.NoError(t, err)
				require.True(t, changed)

				awaitEnable(t, results)
				assert.ElementsMatch(t, test.want, toolNames(t, client.session))
			})
		}
	})

	t.Run("an instance that was ready before and is pending again withdraws the tools", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.learn(t, string(template.UID), template.Generation, "old")

		client := fixture.connectObserved(t, alice())
		results := startEnable(t, client.session, "github")
		instance := fixture.awaitInstance(t, template, "user:alice")
		fixture.becomeReady(t, toolruntime.PostgresStartStore{Pool: fixture.pool}, instance.ID, "old")
		require.Empty(t, awaitEnable(t, results).err)
		require.Contains(t, toolNames(t, client.session), "github__old")

		_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET status = 'pending' WHERE id = $1", instance.ID)
		require.NoError(t, err)

		assert.ElementsMatch(t, metaToolNames, toolNames(t, client.session))
	})
}
