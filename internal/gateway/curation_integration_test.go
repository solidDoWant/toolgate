//go:build integration

package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// curate changes the spec of the catalog's template for server.
func (f *mcpFixture) curate(t *testing.T, server string, change func(*v1alpha1.MCPServerTemplateSpec)) {
	t.Helper()

	template := f.catalogTemplate(t, server).DeepCopy()
	change(&template.Spec)
	f.catalog.Set(template, nil)
}

func TestToolCuration(t *testing.T) {
	t.Run("a hidden tool is absent from the list and refused by either name, recorded and not forwarded", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		upstream := fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo", "fails")
		require.ElementsMatch(t, []string{"github__echo", "github__fails"}, filterPrefix(toolNames(t, client.session), "github__"))

		// A deny list applies at once to the running instance.
		fixture.curate(t, "github", func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"fails"}}
		})
		assert.Equal(t, []string{"github__echo"}, filterPrefix(toolNames(t, client.session), "github__"))

		result := callTool(t, client.session, "github__fails", nil)
		assert.True(t, result.IsError)
		assert.Equal(t, "Server `github` has no tool `fails`.", resultText(result))
		assert.Zero(t, upstream.callCount("fails"))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, "not-offered", decisions[0].Details["reason"])
		assert.Equal(t, "fails", decisions[0].Tool)

		// An allow list hides every other tool.
		fixture.curate(t, "github", func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"fails"}}
		})
		assert.Equal(t, []string{"github__fails"}, filterPrefix(toolNames(t, client.session), "github__"))

		result = callTool(t, client.session, "github__echo", map[string]any{"text": "x"})
		assert.True(t, result.IsError)
		assert.Zero(t, upstream.callCount("echo"))
		assert.Len(t, fixture.auditRows(t, "tool.decision"), 2)
		assert.Empty(t, fixture.auditRows(t, "tool.result"))
	})

	t.Run("a renamed tool is called by its exposed name and reaches the upstream by its own", func(t *testing.T) {
		fixture := newMCPFixture(t)
		fixture.addEnableTemplate("github", "per-user", anyone)
		upstream := fixture.startUpstream(t, "github")

		client := fixture.connectObserved(t, alice())
		fixture.enableWithTools(t, client, "github", "user:alice", "echo")

		fixture.curate(t, "github", func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Tools = map[string]v1alpha1.ToolOverride{"echo": {Name: "say", Description: "Say it."}}
		})

		listed, err := client.session.ListTools(t.Context(), nil)
		require.NoError(t, err)

		var found bool

		for _, tool := range listed.Tools {
			if tool.Name == "github__say" {
				found = true

				assert.Equal(t, "Say it.", tool.Description)
			}

			assert.NotEqual(t, "github__echo", tool.Name)
		}

		require.True(t, found)

		result := callTool(t, client.session, "github__say", map[string]any{"text": "hi"})
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "hi", resultText(result))
		assert.Equal(t, 1, upstream.callCount("echo"))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "echo", decisions[0].Tool)
		assert.Equal(t, "say", decisions[0].Details["exposedTool"])
		assert.Equal(t, "say", fixture.auditRows(t, "tool.result")[0].Details["exposedTool"])

		// The upstream name no longer works.
		result = callTool(t, client.session, "github__echo", map[string]any{"text": "hi"})
		assert.True(t, result.IsError)
		assert.Equal(t, "Server `github` has no tool `echo`.", resultText(result))
		assert.Equal(t, 1, upstream.callCount("echo"))

		decisions = fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 2)
		assert.Equal(t, "not-offered", decisions[1].Details["reason"])
		assert.Equal(t, "say", decisions[1].Details["exposedTool"])
	})

	t.Run("row 199: a rule's tools glob refuses by the upstream name or the exposed name", func(t *testing.T) {
		for _, glob := range []string{"github__echo", "github__say"} {
			fixture := newMCPFixture(t)
			fixture.addEnableTemplate("github", "per-user", anyone)
			upstream := fixture.startUpstream(t, "github")

			client := fixture.connectObserved(t, alice())
			fixture.enableWithTools(t, client, "github", "user:alice", "echo")
			fixture.curate(t, "github", func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.Tools = map[string]v1alpha1.ToolOverride{"echo": {Name: "say"}}
			})

			require.NoError(t, fixture.rules.Set(&v1alpha1.ToolgateRule{
				ObjectMeta: metav1.ObjectMeta{Namespace: templateNamespace, Name: "no-echo"},
				Spec: v1alpha1.ToolgateRuleSpec{
					Match: v1alpha1.RuleMatch{Tools: []string{glob}}, Reason: "Frozen.",
				},
			}))

			assert.Empty(t, filterPrefix(toolNames(t, client.session), "github__"), glob)

			result := callTool(t, client.session, "github__say", map[string]any{"text": "x"})
			assert.True(t, result.IsError, glob)
			assert.Contains(t, resultText(result), "Frozen.", glob)
			assert.Zero(t, upstream.callCount("echo"), glob)
		}
	})
}

func filterPrefix(names []string, prefix string) []string {
	var kept []string

	for _, name := range names {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			kept = append(kept, name)
		}
	}

	return kept
}
