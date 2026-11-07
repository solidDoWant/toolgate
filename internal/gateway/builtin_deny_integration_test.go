//go:build integration

package gateway_test

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
)

const toolDeny = "toolgate_deny"

func TestBuiltinToolsDenied(t *testing.T) {
	for _, test := range []struct {
		name string
		// target is the deny's target argument: workspace or session.
		target   map[string]any
		item     string
		toolOnly bool
		entry    string
		// event is the audit event the deny writes.
		event string
	}{
		{
			name:   "a workspace server-level deny",
			target: map[string]any{"workspace": aliceWorkspace}, item: "github",
			entry: aliceWorkspace + " github  deny",
			event: gateway.TypeWorkspaceChange,
		},
		{
			name:   "a workspace tool-level deny",
			target: map[string]any{"workspace": aliceWorkspace}, item: "github__echo", toolOnly: true,
			entry: aliceWorkspace + " github echo deny",
			event: gateway.TypeWorkspaceChange,
		},
		{
			name:   "a session server-level deny",
			target: map[string]any{"session": "session-1"}, item: "github",
			entry: "session:session-1 github  deny",
			event: gateway.TypeSessionAddition,
		},
		{
			name:   "a session tool-level deny",
			target: map[string]any{"session": "session-1"}, item: "github__echo", toolOnly: true,
			entry: "session:session-1 github echo deny",
			event: gateway.TypeSessionAddition,
		},
	} {
		t.Run(test.name+" withdraws the tools and refuses calls within 5 s across two replicas", func(t *testing.T) {
			fixture := newBuiltinFixture(t)
			// Default all with no list: the deny still narrows.
			fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)

			caller := fixture.connectObserved(t, attestedAlice())
			fixture.enableWithTools(t, caller, "github", "user:alice", "echo", "raw")

			// The same session on another replica, which only the
			// notification tells about the deny.
			other := fixture.replica(t)
			elsewhere := fixture.connectObservedTo(t, other.URL, attestedAlice())
			require.Subset(t, toolNames(t, elsewhere.session), []string{"github__echo", "github__raw"})

			before := elsewhere.listChangedCount()

			arguments := map[string]any{"servers": []string{test.item}}
			for key, value := range test.target {
				arguments[key] = value
			}

			started := time.Now()
			result := callTool(t, caller.session, toolDeny, arguments)
			require.False(t, result.IsError, resultText(result))

			require.Eventually(t, func() bool { return elsewhere.listChangedCount() > before }, scopeChangeBound, 10*time.Millisecond,
				"the session on the other replica got no list_changed within 5 s")
			assert.Less(t, time.Since(started), scopeChangeBound)

			names := toolNames(t, elsewhere.session)
			assert.NotContains(t, names, "github__echo")

			refused := callTool(t, elsewhere.session, "github__echo", map[string]any{"text": "x"})
			require.True(t, refused.IsError)
			assert.Contains(t, resultText(refused), "is denied in")

			decisions := fixture.auditRows(t, "tool.decision")
			last := decisions[len(decisions)-1]
			assert.Equal(t, "refuse", last.Details["decision"])
			assert.Equal(t, policy.ReasonDenied, last.Details["reason"])

			if test.toolOnly {
				assert.Contains(t, names, "github__raw", "a tool-level deny leaves the server's other tools")

				allowed := callTool(t, elsewhere.session, "github__raw", map[string]any{"text": "kept"})
				require.False(t, allowed.IsError, resultText(allowed))
			} else {
				assert.NotContains(t, names, "github__raw")
			}

			assert.Contains(t, fixture.scopeEntryRows(t), test.entry)

			events := fixture.auditRows(t, test.event)
			require.NotEmpty(t, events)
			assert.Contains(t, events[len(events)-1].Details, "denied")

			if test.event == gateway.TypeWorkspaceChange {
				assert.Equal(t, gateway.ViaTool, events[len(events)-1].Details["via"])
			}

			// The deny went through the full call path.
			denyDecisions := slices.DeleteFunc(decisions, func(row auditRow) bool { return row.Tool != "deny" })
			require.Len(t, denyDecisions, 1)
			assert.Equal(t, "allow", denyDecisions[0].Details["decision"])
			assert.Equal(t, gateway.BuiltinServerName, denyDecisions[0].Server)
		})
	}

	t.Run("toolgate__deny names the built-in tool by its upstream name", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		client := fixture.connectObserved(t, attestedAlice())

		result := callTool(t, client.session, toolDeny, map[string]any{"workspace": aliceWorkspace, "servers": []string{"toolgate__deny"}})
		require.False(t, result.IsError, resultText(result))

		assert.Equal(t, []string{aliceWorkspace + " toolgate deny deny"}, fixture.scopeEntryRows(t))
		assert.NotContains(t, toolNames(t, client.session), toolDeny)
		assert.Contains(t, toolNames(t, client.session), toolListScopes)
	})

	t.Run("the input is checked and nothing is written for a refused deny", func(t *testing.T) {
		items := make([]string, 0, 33)
		for index := range 33 {
			items = append(items, "server-"+string(rune('a'+index%26))+string(rune('a'+index/26)))
		}

		for _, test := range []struct {
			name      string
			arguments map[string]any
			message   string
			written   bool
		}{
			{name: "no target", arguments: map[string]any{"servers": []string{"github"}}, message: "Name exactly one of `workspace` and `session`."},
			{name: "both targets", arguments: map[string]any{"workspace": aliceWorkspace, "session": "session-1", "servers": []string{"github"}},
				message: "Name exactly one of `workspace` and `session`."},
			{name: "no items", arguments: map[string]any{"workspace": aliceWorkspace, "servers": []string{}}, message: "Name 1 to 32 servers or tools."},
			{name: "33 items", arguments: map[string]any{"workspace": aliceWorkspace, "servers": items}, message: "Name 1 to 32 servers or tools."},
			{name: "32 items", arguments: map[string]any{"workspace": aliceWorkspace, "servers": items[:32]}, written: true},
			{name: "row 207: another user's session", arguments: map[string]any{"session": "bob-session-1", "servers": []string{"github"}},
				message: "You have no open session `bob-session-1`."},
			{name: "a claimed workspace", arguments: map[string]any{"workspace": "claimed/x", "servers": []string{"github"}},
				message: "Workspace `claimed/x` can't have a list"},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newBuiltinFixture(t)
				fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
				fixture.insertSessions(t, "bob", "bob-session", 1)
				client := fixture.connectObserved(t, attestedAlice())

				result := callTool(t, client.session, toolDeny, test.arguments)
				if test.written {
					require.False(t, result.IsError, resultText(result))
					assert.Len(t, fixture.scopeEntryRows(t), 32)

					return
				}

				require.True(t, result.IsError)
				assert.Contains(t, resultText(result), test.message)
				assert.Empty(t, fixture.scopeEntryRows(t))
			})
		}
	})

	t.Run("an unverified session may deny in itself, and only narrows", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		fixture.setUnverifiedList(t, gateway.BuiltinServerName, "github")
		client := fixture.connectObserved(t, alice())

		result := callTool(t, client.session, toolDeny, map[string]any{"session": "session-1", "servers": []string{"github"}})
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, []string{"session:session-1 github  deny"}, fixture.scopeEntryRows(t))
	})
}
