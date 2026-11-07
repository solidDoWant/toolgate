//go:build integration

package gateway_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	toolruntime "github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// readyRemoteInstance stores a ready instance of the remote template for
// principal, with the tools its upstream listed for that user's credential,
// as the runtime leaves it after an enable.
func (f *mcpFixture) readyRemoteInstance(t *testing.T, template *v1alpha1.MCPServerTemplate, principal string, tools ...*mcp.Tool) {
	t.Helper()

	encoded, err := json.Marshal(tools)
	require.NoError(t, err)

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		instance, err := queries.InsertInstance(t.Context(), db.InsertInstanceParams{
			ID: strings.ReplaceAll(uuid.NewString(), "-", ""), TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: string(template.UID),
			Generation: template.Generation, Scope: "per-user", ScopeKey: "user:" + principal, OwnerPrincipal: principal,
		})
		if err != nil {
			return err
		}

		if _, err := queries.SetInstanceStarting(t.Context(), db.SetInstanceStartingParams{ID: instance.ID, Generation: instance.Generation}); err != nil {
			return err
		}

		_, err = queries.SetInstanceReady(t.Context(), db.SetInstanceReadyParams{ID: instance.ID, Tools: encoded})

		return err
	}))
}

func remoteTool(name, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, InputSchema: map[string]any{"type": "object"}}
}

// rebuildSearchRows rebuilds template's search rows, as the leader does.
func (f *mcpFixture) rebuildSearchRows(t *testing.T, template *v1alpha1.MCPServerTemplate) {
	t.Helper()

	require.NoError(t, pgx.BeginFunc(t.Context(), f.pool, func(tx pgx.Tx) error {
		return toolruntime.RebuildToolSearch(t.Context(), tx, string(template.UID), template)
	}))
}

func TestRemoteServerToolsInListAndSearch(t *testing.T) {
	t.Run("declared tools are listed and searchable before any enable", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addRemoteTemplate("mail-archiver")
		fixture.declareTools(t, template, declared("search_mail", "Search the archived mail messages"))

		client := fixture.connect(t, searching(alice()))
		found, _ := findTools(t, client, "search archived mail", 0)

		assert.Equal(t, []string{"mail-archiver__search_mail"}, foundNames(found))
		assert.Contains(t, toolNames(t, client), "mail-archiver__search_mail")
	})

	t.Run("without declared tools, a user finds only the tools of their own enabled instance", func(t *testing.T) {
		fixture := newMCPFixture(t)
		template := fixture.addRemoteTemplate("mail-archiver")
		template.Spec.DeclaredTools = nil
		fixture.catalog.Set(template, nil)

		// A learned list, as a launched generation of the template left it, is
		// never the remote template's catalog list.
		_, err := fixture.pool.Exec(t.Context(),
			`INSERT INTO tool_catalog (template_uid, generation, template_ns, template_name, tools, instance_id)
			VALUES ($1, $2, $3, $4, $5, 'old')`,
			string(template.UID), template.Generation, template.Namespace, template.Name, `[{"name":"old_launched_tool","description":"archived mail from the old server","inputSchema":{"type":"object"}}]`)
		require.NoError(t, err)

		fixture.rebuildSearchRows(t, template)

		aliceSession := fixture.connect(t, searching(alice()))
		bobSession := fixture.connect(t, searching(bob()))

		found, _ := findTools(t, aliceSession, "archived mail", 20)
		assert.Empty(t, foundNames(found), "nothing is searchable before the user enables the server")

		fixture.readyRemoteInstance(t, template, "alice",
			remoteTool("search_mail", "Search the archived mail messages"),
			remoteTool("alice_folders", "List the folders of the archived mail"))

		found, _ = findTools(t, bobSession, "archived mail folders", 20)
		assert.Empty(t, foundNames(found), "another user's tools are never found")
		assert.NotContains(t, toolNames(t, bobSession), "mail-archiver__alice_folders")

		fixture.readyRemoteInstance(t, template, "bob",
			remoteTool("search_mail", "Search the archived mail messages"),
			remoteTool("bob_labels", "List the labels of the archived mail"))

		found, _ = findTools(t, aliceSession, "archived mail folders labels", 20)
		assert.ElementsMatch(t, []string{"mail-archiver__search_mail", "mail-archiver__alice_folders"}, foundNames(found))

		found, _ = findTools(t, bobSession, "archived mail folders labels", 20)
		assert.ElementsMatch(t, []string{"mail-archiver__search_mail", "mail-archiver__bob_labels"}, foundNames(found))

		aliceTools, bobTools := toolNames(t, aliceSession), toolNames(t, bobSession)
		assert.Contains(t, aliceTools, "mail-archiver__alice_folders")
		assert.NotContains(t, aliceTools, "mail-archiver__bob_labels")
		assert.NotContains(t, aliceTools, "mail-archiver__old_launched_tool")
		assert.Contains(t, bobTools, "mail-archiver__bob_labels")
		assert.NotContains(t, bobTools, "mail-archiver__alice_folders")

		var searchRows int
		require.NoError(t, fixture.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM tool_search WHERE template_uid = $1`, string(template.UID)).Scan(&searchRows))
		assert.Zero(t, searchRows, "a remote server's tools have no shared search rows")
	})
}
