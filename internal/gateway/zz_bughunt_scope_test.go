//go:build integration

package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// A deny of a renamed tool by its exposed name, made while the template is
// invalid (its last valid generation still runs, row 246), must refuse the
// tool: scope entries hold upstream names (§2, row 233).
func TestBughuntScopeDenyRenamedToolWhileTemplateInvalid(t *testing.T) {
	fixture := newAdditionFixture(t)
	api := newWorkspaceAPI(t, fixture, true)
	fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)

	template := fixture.catalogTemplate(t, "github")
	template.Spec.Tools = map[string]v1alpha1.ToolOverride{"echo": {Name: "say"}}
	fixture.catalog.Set(template, nil)

	client := fixture.connectObserved(t, attestedAlice())
	fixture.enableWithTools(t, client, "github", "user:alice", "echo")

	before := callTool(t, client.session, "github__say", map[string]any{"text": "hi"})
	require.False(t, before.IsError, resultText(before))

	// The template's newest generation becomes invalid; calls go on with
	// the last valid one.
	invalid := template.DeepCopy()
	invalid.Generation++
	fixture.catalog.Set(invalid, []string{"broken"})

	api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github__say"}})

	t.Logf("scope entries: %v", fixture.scopeEntryRows(t))
	assert.Equal(t, []string{aliceWorkspace + " github echo deny"}, fixture.scopeEntryRows(t))

	after := callTool(t, client.session, "github__say", map[string]any{"text": "hi"})
	assert.True(t, after.IsError, "the denied tool was still called: %s", resultText(after))
}

// Row 251: a session that enabled a real template named toolgate before the
// upgrade keeps its session_servers row. The name now means the built-in
// server, so its tools must reach the built-in server.
func TestBughuntScopeLegacyToolgateRowShadowsBuiltin(t *testing.T) {
	fixture := newBuiltinFixture(t)
	fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)

	legacy := fixture.addTemplate("toolgate", "per-user", aliceAndBob)
	fixture.catalog.Set(legacy, []string{"The server name `toolgate` is reserved for the built-in server."})

	client := fixture.connectObserved(t, attestedAlice())
	listServers(t, client.session)

	_, err := fixture.pool.Exec(t.Context(), `INSERT INTO session_servers (session_pk, template_ns, template_name)
		SELECT id, $1, 'toolgate' FROM sessions WHERE principal = 'alice'`, templateNamespace)
	require.NoError(t, err)

	result := callTool(t, client.session, toolListScopes, nil)
	assert.False(t, result.IsError, resultText(result))
}

// Same as above, with an explicit enable of toolgate first.
func TestBughuntScopeLegacyToolgateRowAfterEnable(t *testing.T) {
	fixture := newBuiltinFixture(t)
	fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)

	legacy := fixture.addTemplate("toolgate", "per-user", aliceAndBob)
	fixture.catalog.Set(legacy, []string{"The server name `toolgate` is reserved for the built-in server."})

	client := fixture.connectObserved(t, attestedAlice())
	listServers(t, client.session)

	_, err := fixture.pool.Exec(t.Context(), `INSERT INTO session_servers (session_pk, template_ns, template_name)
		SELECT id, $1, 'toolgate' FROM sessions WHERE principal = 'alice'`, templateNamespace)
	require.NoError(t, err)

	enabled := callTool(t, client.session, "toolgate_enable_server", map[string]any{"name": "toolgate"})
	t.Logf("enable: error=%v %s", enabled.IsError, resultText(enabled))

	result := callTool(t, client.session, toolListScopes, nil)
	assert.False(t, result.IsError, resultText(result))
}

// Row 247 / row 240: a session ended by a 0.3 leader keeps its allow
// entries until the 0.4 leader's pass. If the session restarts (resume)
// before that pass, the entries must still not apply to the restarted
// session, and the pass must still remove them.
func TestBughuntScopeAllowEntriesSurviveRestartAfterLegacyEnd(t *testing.T) {
	fixture := newAdditionFixture(t)
	fixture.setVerifiedDefault(t, policy.ScopeDefaultNone)

	first := fixture.connectObserved(t, attestedAlice())
	assertBlockedInWorkspace(t, listServers(t, first.session), "github")

	fixture.addSessionEntry(t, "session-1", "github", "echo", policy.EffectAllow)
	assertNotBlocked(t, listServers(t, first.session), "github")

	// A 0.3 leader ends the session: it sets ended_at and deletes the
	// session additions, and knows nothing of scope_entries.
	_, err := fixture.pool.Exec(t.Context(), `UPDATE sessions SET ended_at = now() WHERE principal = 'alice' AND session_id = 'session-1'`)
	require.NoError(t, err)
	_, err = fixture.pool.Exec(t.Context(), `DELETE FROM session_additions`)
	require.NoError(t, err)

	// The client comes back with the same session ID: the session restarts.
	second := fixture.connectObserved(t, attestedAlice())
	listServers(t, second.session)

	var restarted bool
	require.NoError(t, fixture.pool.QueryRow(t.Context(), `SELECT ended_at IS NULL FROM sessions WHERE principal = 'alice' AND session_id = 'session-1'`).Scan(&restarted))
	require.True(t, restarted, "the session restarted")

	// The 0.4 leader's 30 s pass runs.
	require.NoError(t, store.DeleteStaleRows(t.Context(), db.New(fixture.pool)))

	t.Logf("scope entries after restart and pass: %v", fixture.scopeEntryRows(t))
	assert.Empty(t, fixture.scopeEntryRows(t), "the ended session's allow entry is still stored")
	assertBlockedInWorkspace(t, listServers(t, second.session), "github")
}

// §7: rules may name the built-in tools as `toolgate__list_scopes` or
// `toolgate_list_scopes`. Both must refuse the call.
func TestBughuntScopeRuleNamingBuiltinExposedName(t *testing.T) {
	for _, name := range []string{"toolgate__list_scopes", "toolgate_list_scopes"} {
		t.Run(name, func(t *testing.T) {
			fixture := newBuiltinFixture(t)
			fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
			require.NoError(t, fixture.rules.Set(blockToolFor("alice", name, "No scopes for alice.")))

			client := fixture.connectObserved(t, attestedAlice())

			result := callTool(t, client.session, toolListScopes, nil)
			assert.True(t, result.IsError, "rule %q didn't refuse: %.80s", name, resultText(result))
		})
	}
}
