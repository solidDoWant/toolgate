//go:build integration

package gateway_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

const toolAllow = "toolgate_allow"

// allowResult decodes a toolgate_allow result.
func allowResult(t *testing.T, result *mcp.CallToolResult) gateway.AllowResult {
	t.Helper()

	require.False(t, result.IsError, resultText(result))

	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)

	var decoded gateway.AllowResult
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	return decoded
}

// setAccessRequestStatus moves a pending access request to status and
// notifies approval:<id>, as the approval page on another replica does.
func (f *mcpFixture) setAccessRequestStatus(t *testing.T, id, status string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), `WITH changed AS (
			UPDATE access_requests SET status = $2 WHERE id = $1 AND status = 'pending' RETURNING id)
		SELECT pg_notify('toolgate_events', 'approval:' || id) FROM changed`, id, status)
	require.NoError(t, err)
}

// connectAllowing connects user with a client that answers every
// elicitation with answer, and enables the built-in server so the session
// offers its tools through the go-sdk's tool path.
func (f *mcpFixture) connectAllowing(t *testing.T, user user, answer string) *elicitingClient {
	t.Helper()

	client := f.connectElicitingAs(t, user, elicitingOptions{revision: revisionElicitCreate, answer: answer})

	enabled := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": gateway.BuiltinServerName})
	require.False(t, enabled.IsError, resultText(enabled))

	tools, err := client.session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}

	require.Contains(t, names, toolAllow)

	return client
}

// startAllow calls toolgate_allow in the background.
func startAllow(t *testing.T, client *elicitingClient, arguments map[string]any) <-chan callOutcome {
	t.Helper()

	params := &mcp.CallToolParams{Name: toolAllow, Arguments: arguments}
	params.SetProgressToken("allow")

	return startEcho(t.Context(), client.session, params)
}

func TestBuiltinAllow(t *testing.T) {
	t.Run("without elicitation an allow inserts one pending scope request, returns its link and changes nothing", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		curatedGithub(fixture)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		client := fixture.connectObserved(t, attestedAlice())

		arguments := map[string]any{"workspace": "coder/alice/other", "servers": []string{"github__open_issue"}}
		result := callTool(t, client.session, toolAllow, arguments)
		pending := allowResult(t, result)

		assert.Equal(t, "pending", pending.Status)
		assert.Equal(t, approvalBaseURL+pending.Request, pending.URL)
		assert.Contains(t, resultText(result), "Approve this change at "+pending.URL+".")

		kind, status, sessionPK := fixture.accessRequestRow(t, pending.Request)
		assert.Equal(t, "scope", kind)
		assert.Equal(t, "pending", status)
		assert.Nil(t, sessionPK)
		require.Len(t, fixture.auditRows(t, gateway.TypeAccessRequest), 1)
		assert.Empty(t, fixture.scopeEntryRows(t), "nothing changes before approval")

		again := allowResult(t, callTool(t, client.session, toolAllow, arguments))
		assert.Equal(t, pending.Request, again.Request, "the pending request with the same target and change is found")
		assert.Equal(t, 1, fixture.accessRequestCount(t))
	})

	t.Run("an accepted elicitation's round trip finds the named request and waits until it is approved", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		curatedGithub(fixture)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		client := fixture.connectAllowing(t, attestedAlice(), "accept")
		fixture.connectObserved(t, attestedAliceIn("session-2"))

		outcomes := startAllow(t, client, map[string]any{"session": "session-2", "servers": []string{"github"}})
		fixture.awaitWaiting(t, client)

		require.Equal(t, 1, fixture.accessRequestCount(t), "the round trip inserts no second request")
		require.Len(t, client.elicitations(), 1)

		elicitation := client.elicitations()[0]
		id := elicitation.ElicitationID
		assert.Equal(t, "url", elicitation.Mode)
		assert.Equal(t, approvalBaseURL+id, elicitation.URL)
		assert.Equal(t, 1, gateway.WaitingCalls(fixture.handlers[0], "alice"), "the wait counts toward the principal's waits")

		started := time.Now()

		api := newWorkspaceAPI(t, fixture, true)
		require.NoError(t, api.approveScope(t, aliceIdentity(), id))

		outcome := awaitOutcome(t, outcomes, approvalWakeBound)
		require.NoError(t, outcome.err)
		assert.Less(t, time.Since(started), approvalWakeBound)

		approved := allowResult(t, outcome.result)
		assert.Equal(t, gateway.AllowResult{Status: "approved", Request: id, URL: approvalBaseURL + id}, approved)
		assert.Equal(t, []string{"github"}, fixture.sessionAdditions(t, fixture.sessionRow(t, "alice", "session-2").ID))
		assert.Zero(t, gateway.WaitingCalls(fixture.handlers[0], "alice"))
	})

	for _, test := range []struct {
		name   string
		status string
		// overdue makes the request's approvalTimeout pass instead.
		overdue bool
	}{
		{name: "a denied request returns denied", status: "denied"},
		{name: "a request whose approvalTimeout passes is expired by the wait, with access.expire", status: "expired", overdue: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBuiltinFixture(t)
			curatedGithub(fixture)
			fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
			client := fixture.connectAllowing(t, attestedAlice(), "accept")

			outcomes := startAllow(t, client, map[string]any{"workspace": "coder/alice/other", "servers": []string{"github"}})
			fixture.awaitWaiting(t, client)

			id := client.elicitations()[0].ElicitationID

			if test.overdue {
				_, err := fixture.pool.Exec(t.Context(), `UPDATE access_requests SET created_at = now() - interval '1 hour', expires_at = now() - interval '1 second' WHERE id = $1`, id)
				require.NoError(t, err)
				_, err = fixture.pool.Exec(t.Context(), `SELECT pg_notify('toolgate_events', 'approval:' || $1::text)`, id)
				require.NoError(t, err)
			} else {
				fixture.setAccessRequestStatus(t, id, test.status)
			}

			outcome := awaitOutcome(t, outcomes, approvalWakeBound)
			require.NoError(t, outcome.err)
			assert.Equal(t, test.status, allowResult(t, outcome.result).Status)

			_, status, _ := fixture.accessRequestRow(t, id)
			assert.Equal(t, test.status, status)
			assert.Empty(t, fixture.scopeEntryRows(t))

			if test.overdue {
				assert.Len(t, fixture.auditRows(t, "access.expire"), 1)
			}
		})
	}

	t.Run("a declined elicitation returns pending and keeps the request", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		curatedGithub(fixture)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		client := fixture.connectAllowing(t, attestedAlice(), "decline")

		result := callTool(t, client.session, toolAllow, map[string]any{"workspace": "coder/alice/other", "servers": []string{"github"}})
		pending := allowResult(t, result)
		assert.Equal(t, "pending", pending.Status)
		require.Len(t, client.elicitations(), 1)
		assert.Equal(t, pending.Request, client.elicitations()[0].ElicitationID)

		_, status, _ := fixture.accessRequestRow(t, pending.Request)
		assert.Equal(t, "pending", status)
	})

	t.Run("a named request that was denied or expired gets the denied or expired messages", func(t *testing.T) {
		for _, test := range []struct {
			status  string
			message string
		}{
			{status: "denied", message: "Adding `github` was denied (request `%s`)."},
			{status: "expired", message: "Nobody allowed `github` within 10 minutes (request `%s`)."},
		} {
			t.Run(test.status, func(t *testing.T) {
				fixture := newBuiltinFixture(t)
				curatedGithub(fixture)
				fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
				client := fixture.connectObserved(t, attestedAlice())

				arguments := map[string]any{"workspace": "coder/alice/other", "servers": []string{"github"}}
				id := allowResult(t, callTool(t, client.session, toolAllow, arguments)).Request
				fixture.setAccessRequestStatus(t, id, test.status)

				result, err := client.session.CallTool(t.Context(), &mcp.CallToolParams{Name: toolAllow, Arguments: arguments, RequestState: id})
				require.NoError(t, err)
				require.True(t, result.IsError)
				assert.Equal(t, fmtMessage(test.message, id), resultText(result))
				assert.Equal(t, 1, fixture.accessRequestCount(t), "no second request is inserted")
			})
		}
	})

	t.Run("the input and the target are checked and nothing is written for a refused allow", func(t *testing.T) {
		items := make([]string, 0, 33)
		for range 33 {
			items = append(items, "github")
		}

		for _, test := range []struct {
			name      string
			arguments map[string]any
			// deny is a server-level deny entry on the target, as
			// `<workspace or session:ID> server`.
			deny    string
			message string
		}{
			{name: "no target", arguments: map[string]any{"servers": []string{"github"}}, message: "Name exactly one of `workspace` and `session`."},
			{name: "both targets", arguments: map[string]any{"workspace": aliceWorkspace, "session": "session-1", "servers": []string{"github"}},
				message: "Name exactly one of `workspace` and `session`."},
			{name: "no items", arguments: map[string]any{"workspace": aliceWorkspace, "servers": []string{}}, message: "Name 1 to 32 servers or tools."},
			{name: "33 items", arguments: map[string]any{"workspace": aliceWorkspace, "servers": items}, message: "Name 1 to 32 servers or tools."},
			{name: "a server without access", arguments: map[string]any{"workspace": aliceWorkspace, "servers": []string{"bobonly"}},
				message: "You don't have access to server `bobonly`."},
			{name: "a tool the server doesn't offer", arguments: map[string]any{"workspace": aliceWorkspace, "servers": []string{"github__create_issue"}},
				message: "Server `github` has no tool `create_issue`."},
			{name: "a claimed workspace", arguments: map[string]any{"workspace": "claimed/x", "servers": []string{"github"}},
				message: "Workspace `claimed/x` can't have a list"},
			{name: "another user's session", arguments: map[string]any{"session": "bob-session-1", "servers": []string{"github"}},
				message: "You have no open session `bob-session-1`."},
			{name: "an unverified session", arguments: map[string]any{"session": "unverified", "servers": []string{"github"}},
				message: "Session `unverified` has no verified workspace, so it uses only your default for unverified sessions."},
			{name: "a tool of a server denied in the workspace", deny: "workspace",
				arguments: map[string]any{"workspace": aliceWorkspace, "servers": []string{"github__open_issue"}},
				message:   "Server `github` is denied in workspace `" + aliceWorkspace + "`. Allow the server first."},
			{name: "a tool of a server denied in the session", deny: "session",
				arguments: map[string]any{"session": "session-2", "servers": []string{"github__open_issue"}},
				message:   "Server `github` is denied in this session. Allow the server first."},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newBuiltinFixture(t)
				curatedGithub(fixture)
				bobOnly := fixture.addEnableTemplate("bobonly", "per-user", aliceAndBob)
				bobOnly.Spec.Access.Users = []string{"bob"}
				fixture.catalog.Set(bobOnly, nil)
				fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
				fixture.insertSessions(t, "bob", "bob-session", 1)

				unverified := alice()
				unverified.session = "unverified"
				fixture.connectObserved(t, unverified)
				fixture.connectObserved(t, attestedAliceIn("session-2"))

				client := fixture.connectObserved(t, attestedAlice())

				var entriesBefore []string

				if test.deny != "" {
					deny := map[string]any{"workspace": aliceWorkspace, "servers": []string{"github"}}
					if test.deny == "session" {
						deny = map[string]any{"session": "session-2", "servers": []string{"github"}}
					}

					require.False(t, callTool(t, client.session, toolDeny, deny).IsError)

					entriesBefore = fixture.scopeEntryRows(t)
				}

				result := callTool(t, client.session, toolAllow, test.arguments)
				require.True(t, result.IsError)
				assert.Contains(t, resultText(result), test.message)
				assert.Zero(t, fixture.accessRequestCount(t))
				assert.ElementsMatch(t, entriesBefore, fixture.scopeEntryRows(t))
				assert.Empty(t, fixture.auditRows(t, gateway.TypeAccessRequest))

				results := fixture.auditRows(t, "tool.result")
				assert.Equal(t, "error", results[len(results)-1].Details["outcome"], "the call's tool.result is an error")
			})
		}
	})

	t.Run("an ended session gets the no-open-session message", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		curatedGithub(fixture)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		fixture.connectObserved(t, attestedAliceIn("session-2"))
		fixture.endIdleSession(t, "session-2")
		client := fixture.connectObserved(t, attestedAlice())

		result := callTool(t, client.session, toolAllow, map[string]any{"session": "session-2", "servers": []string{"github"}})
		require.True(t, result.IsError)
		assert.Equal(t, "You have no open session `session-2`.", resultText(result))
		assert.Zero(t, fixture.accessRequestCount(t))
	})
}

func TestBuiltinToolsAllowed(t *testing.T) {
	calls := []struct {
		tool      string
		arguments map[string]any
	}{
		{tool: toolListScopes},
		{tool: toolAllow, arguments: map[string]any{"workspace": "coder/alice/other", "servers": []string{"github"}}},
		{tool: toolDeny, arguments: map[string]any{"workspace": "coder/alice/other", "servers": []string{"github"}}},
	}

	for _, call := range calls {
		t.Run(call.tool+" works in a session that allows toolgate", func(t *testing.T) {
			fixture := newBuiltinFixture(t)
			curatedGithub(fixture)
			fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
			client := fixture.connectObserved(t, attestedAlice())

			result := callTool(t, client.session, call.tool, call.arguments)
			require.False(t, result.IsError, resultText(result))

			decisions := fixture.auditRows(t, "tool.decision")
			require.Len(t, decisions, 1)
			assert.Equal(t, "allow", decisions[0].Details["decision"])
			assert.Equal(t, gateway.BuiltinServerName, decisions[0].Server)
			require.Len(t, fixture.auditRows(t, "tool.result"), 1)
		})

		t.Run(""+call.tool+" is refused at step 4 where toolgate isn't allowed, with tool.decision", func(t *testing.T) {
			fixture := newBuiltinFixture(t)
			curatedGithub(fixture)
			// A verified workspace whose list doesn't hold toolgate.
			fixture.setVerifiedDefault(t, policy.ScopeDefaultNone)
			client := fixture.connectObserved(t, attestedAlice())

			result := callTool(t, client.session, call.tool, call.arguments)
			require.True(t, result.IsError)

			decisions := fixture.auditRows(t, "tool.decision")
			require.Len(t, decisions, 1)
			assert.Equal(t, gateway.BuiltinServerName, decisions[0].Server)
			assert.Equal(t, "refuse", decisions[0].Details["decision"])
			assert.Equal(t, policy.StepWorkspaceScope, decisions[0].Details["step"])
			assert.Empty(t, fixture.auditRows(t, "tool.result"))
			assert.Empty(t, fixture.scopeEntryRows(t))
		})

		t.Run(call.tool+" is refused by a rule naming toolgate", func(t *testing.T) {
			fixture := newBuiltinFixture(t)
			curatedGithub(fixture)
			fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
			require.NoError(t, fixture.rules.Set(blockServer(gateway.BuiltinServerName)))
			client := fixture.connectObserved(t, attestedAlice())

			result := callTool(t, client.session, call.tool, call.arguments)
			require.True(t, result.IsError)

			decisions := fixture.auditRows(t, "tool.decision")
			require.Len(t, decisions, 1)
			assert.Equal(t, policy.StepRule, decisions[0].Details["step"])
			assert.Empty(t, fixture.scopeEntryRows(t))
			assert.Zero(t, fixture.accessRequestCount(t))
		})
	}

	for _, tool := range []string{toolAllow, toolDeny} {
		t.Run(""+tool+" refuses another user's session and writes nothing", func(t *testing.T) {
			fixture := newBuiltinFixture(t)
			curatedGithub(fixture)
			fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
			fixture.connectObserved(t, bob())
			client := fixture.connectObserved(t, attestedAliceIn("session-2"))

			// bob's session is named session-1; alice has none of that name.
			result := callTool(t, client.session, tool, map[string]any{"session": "session-1", "servers": []string{"github"}})
			require.True(t, result.IsError)
			assert.Equal(t, "You have no open session `session-1`.", resultText(result))
			assert.Empty(t, fixture.scopeEntryRows(t))
			assert.Zero(t, fixture.accessRequestCount(t))

			results := fixture.auditRows(t, "tool.result")
			require.Len(t, results, 1)
			assert.Equal(t, "error", results[0].Details["outcome"])
		})
	}

	t.Run("the same allow through the tool and the API makes the same request", func(t *testing.T) {
		for _, target := range []map[string]any{
			{"workspace": "coder/alice/other"},
			{"session": "session-2"},
		} {
			fixture := newBuiltinFixture(t)
			curatedGithub(fixture)
			fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
			api := newWorkspaceAPI(t, fixture, true)
			fixture.connectObserved(t, attestedAliceIn("session-2"))
			client := fixture.connectObserved(t, attestedAlice())

			arguments := map[string]any{"servers": []string{"github__open_issue"}}
			change := workspacesapi.Change{Action: "allow", Servers: []string{"github__open_issue"}}

			for key, value := range target {
				arguments[key] = value
			}

			if session, ok := target["session"].(string); ok {
				change.Session = session
			} else {
				change.Workspace = target["workspace"].(string)
			}

			viaTool := allowResult(t, callTool(t, client.session, toolAllow, arguments)).Request
			viaAPI := api.requestChange(t, attestedAlice(), change)
			assert.Equal(t, viaTool, viaAPI, "target %v", target)
			assert.Equal(t, 1, fixture.accessRequestCount(t))
		}
	})

	t.Run("rows 243, 244: the same deny through the tool and the API gives the same rows for every target, and a clear after them", func(t *testing.T) {
		scenarios := []struct {
			name   string
			listed bool
			steps  []workspacesapi.Change
		}{
			{name: "a workspace with no list", steps: []workspacesapi.Change{
				{Action: "deny", Servers: []string{"github"}},
				{Action: "deny", Servers: []string{"slack__echo"}},
			}},
			{name: "a listed workspace", listed: true, steps: []workspacesapi.Change{
				{Action: "deny", Servers: []string{"github"}},
				{Action: "deny", Servers: []string{"slack__echo"}},
			}},
			{name: "a session", steps: []workspacesapi.Change{
				{Action: "deny", Session: "session-2", Servers: []string{"github"}},
				{Action: "deny", Session: "session-2", Servers: []string{"slack__echo"}},
			}},
		}

		for _, scenario := range scenarios {
			rows := map[bool][]string{}
			lists := map[bool]map[string][]string{}

			// Each change goes through the tool, then in a new fixture
			// through the API; a clear always goes through the API.
			for _, viaTool := range []bool{true, false} {
				fixture := newBuiltinFixture(t)
				curatedGithub(fixture)
				fixture.addEnableTemplate("slack", "per-user", aliceAndBob)
				fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
				api := newWorkspaceAPI(t, fixture, true)

				if scenario.listed {
					api.widen(t, attestedAlice(), workspacesapi.Change{Action: "allow", Servers: []string{"github", "slack", gateway.BuiltinServerName}})
				}

				fixture.connectObserved(t, attestedAliceIn("session-2"))
				client := fixture.connectObserved(t, attestedAlice())

				for _, change := range scenario.steps {
					if !viaTool {
						api.applyChange(t, attestedAlice(), change)

						continue
					}

					arguments := map[string]any{"servers": change.Servers}
					if change.Session != "" {
						arguments["session"] = change.Session
					} else {
						arguments["workspace"] = aliceWorkspace
					}

					result := callTool(t, client.session, toolDeny, arguments)
					require.False(t, result.IsError, resultText(result))
				}

				if scenario.listed {
					// With verified_default all, a clear widens, so it
					// goes through a change request.
					id := api.requestChange(t, attestedAlice(), workspacesapi.Change{Action: "clear"})
					require.NoError(t, api.approve(t, id))
				}

				rows[viaTool] = fixture.scopeEntryRows(t)
				lists[viaTool] = api.lists(t, attestedAlice())
			}

			assert.NotEmpty(t, rows[true], scenario.name)
			assert.Equal(t, rows[true], rows[false], scenario.name)
			assert.Equal(t, lists[true], lists[false], scenario.name)

			for _, row := range rows[true] {
				assert.Contains(t, row, " deny", "%s: a clear keeps the deny entries", scenario.name)
			}
		}
	})
}

func fmtMessage(format, id string) string {
	return fmt.Sprintf(format, id)
}
