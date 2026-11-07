//go:build integration

package gateway_test

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// sayWhileInvalid exposes github's upstream echo as say, then makes the
// template's newest generation invalid, so calls go on with the last valid
// generation.
func sayWhileInvalid(t *testing.T, fixture *mcpFixture) *observedClient {
	t.Helper()

	template := fixture.catalogTemplate(t, "github")
	template.Spec.Tools = map[string]v1alpha1.ToolOverride{"echo": {Name: "say"}}
	fixture.catalog.Set(template, nil)

	client := fixture.connectObserved(t, attestedAlice())
	fixture.enableWithTools(t, client, "github", "user:alice", "echo")

	before := callTool(t, client.session, "github__say", map[string]any{"text": "hi"})
	require.False(t, before.IsError, resultText(before))

	invalid := template.DeepCopy()
	invalid.Generation++
	fixture.catalog.Set(invalid, []string{"broken"})

	return client
}

// assertSayDenied checks that github__say is withdrawn and its calls are
// refused as denied within 5 s.
func assertSayDenied(t *testing.T, client *observedClient) {
	t.Helper()

	require.Eventually(t, func() bool {
		return !slices.Contains(toolNames(t, client.session), "github__say")
	}, scopeChangeBound, 10*time.Millisecond, "github__say is still listed")

	refused := callTool(t, client.session, "github__say", map[string]any{"text": "hi"})
	require.True(t, refused.IsError, "the denied tool was still called: %s", resultText(refused))
	assert.Contains(t, resultText(refused), "is denied in")
}

// Rows 233 and 246: while a template's current generation is invalid, a
// scope item naming a renamed tool by its exposed name maps to the upstream
// name with the last valid generation, as calls do.
func TestScopeItemsOfRenamedToolWhileTemplateInvalid(t *testing.T) {
	t.Run("an API deny stores the upstream name and refuses the tool", func(t *testing.T) {
		fixture := newAdditionFixture(t)
		api := newWorkspaceAPI(t, fixture, true)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		client := sayWhileInvalid(t, fixture)

		api.applyChange(t, attestedAlice(), workspacesapi.Change{Action: "deny", Servers: []string{"github__say"}})

		assert.Equal(t, []string{aliceWorkspace + " github echo deny"}, fixture.scopeEntryRows(t))
		assert.Equal(t, []string{"github__say"}, workspaceNamed(t, api.list(t, attestedAlice()), aliceWorkspace).Denied,
			"the entry is shown by its exposed name")
		assertSayDenied(t, client)
	})

	t.Run("toolgate_deny stores the upstream name and refuses the tool", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		client := sayWhileInvalid(t, fixture)

		result := callTool(t, client.session, toolDeny, map[string]any{"workspace": aliceWorkspace, "servers": []string{"github__say"}})
		require.False(t, result.IsError, resultText(result))

		assert.Equal(t, []string{aliceWorkspace + " github echo deny"}, fixture.scopeEntryRows(t))
		assertSayDenied(t, client)
	})

	t.Run("toolgate_allow requests the upstream name", func(t *testing.T) {
		fixture := newBuiltinFixture(t)
		api := newWorkspaceAPI(t, fixture, true)
		fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
		client := sayWhileInvalid(t, fixture)

		result := callTool(t, client.session, toolAllow, map[string]any{"workspace": "coder/alice/other", "servers": []string{"github__say"}})
		require.False(t, result.IsError, resultText(result))

		pending := allowResult(t, result)

		request, err := api.workspaces.AccessRequest(t.Context(), "alice", pending.Request)
		require.NoError(t, err)

		change, err := gateway.DecodeScopeChange(request.Change)
		require.NoError(t, err)
		assert.Equal(t, []gateway.ScopeItem{{Server: "github", Tool: "echo"}}, change.Items)
	})
}
