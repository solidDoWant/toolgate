//go:build integration

package gateway_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	toolruntime "github.com/solidDoWant/toolgate/internal/runtime"
)

// A rule or temporary block names a built-in tool by its exact name or as
// toolgate__<tool>. Either refuses its
// calls and leaves it out of tools/list and toolgate_find_tools.
func TestBuiltinToolRules(t *testing.T) {
	for _, test := range []struct {
		name    string
		glob    string
		refused bool
	}{
		{name: "exact name", glob: toolListScopes, refused: true},
		{name: "server and upstream name", glob: "toolgate__list_scopes", refused: true},
		{name: "another tool's exact name", glob: toolDeny},
	} {
		for _, kind := range []struct {
			name string
			step string
			add  func(t *testing.T, fixture *mcpFixture, glob string)
		}{
			{name: "rule", step: policy.StepRule, add: func(t *testing.T, fixture *mcpFixture, glob string) {
				require.NoError(t, fixture.rules.Set(blockToolFor("alice", glob, "Not today.")))
			}},
			{name: "temporary block", step: policy.StepTemporaryBlock, add: func(t *testing.T, fixture *mcpFixture, glob string) {
				fixture.insertBlock(t, v1alpha1.RuleMatch{Tools: []string{glob}}, "Not today.", time.Hour, true)
			}},
		} {
			t.Run(kind.name+" naming "+test.name, func(t *testing.T) {
				fixture := newBuiltinFixture(t)
				fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)

				builtin := gateway.BuiltinTemplate()

				require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
					return toolruntime.RebuildToolSearchEmbedded(t.Context(), tx, string(builtin.UID), builtin, fixture.embedder, nil)
				}))
				kind.add(t, fixture, test.glob)

				client := fixture.connectObserved(t, attestedAlice())

				enabled := callTool(t, client.session, gateway.ToolEnableServer, map[string]any{"name": gateway.BuiltinServerName})
				require.False(t, enabled.IsError, resultText(enabled))

				listed := toolNames(t, client.session)
				assert.Contains(t, listed, toolAllow)
				assert.Equal(t, !test.refused, contains(listed, toolListScopes), "tools/list: %v", listed)

				found, result := findTools(t, fixture.connect(t, searching(attestedAlice())), "list your workspace scope", 10)
				require.False(t, result.IsError, resultText(result))
				assert.Equal(t, !test.refused, contains(foundNames(found), toolListScopes), "toolgate_find_tools: %v", foundNames(found))

				called := callTool(t, client.session, toolListScopes, nil)
				assert.Equal(t, test.refused, called.IsError, resultText(called))

				decisions := fixture.auditRows(t, "tool.decision")
				require.Len(t, decisions, 1)
				assert.Equal(t, gateway.BuiltinServerName, decisions[0].Server)
				assert.Equal(t, "list_scopes", decisions[0].Tool)

				if test.refused {
					assert.Equal(t, "refuse", decisions[0].Details["decision"])
					assert.Equal(t, kind.step, decisions[0].Details["step"])
					assert.Contains(t, resultText(called), "Not today.")
				} else {
					assert.Equal(t, "allow", decisions[0].Details["decision"])
				}
			})
		}
	}
}
