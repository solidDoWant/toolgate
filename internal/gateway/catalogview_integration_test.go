//go:build integration

package gateway_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// insertOwnedInstance stores a live instance of template owned by owner
// (empty for shared) and returns its ID.
func (f *mcpFixture) insertOwnedInstance(t *testing.T, template *v1alpha1.MCPServerTemplate, owner string, generation int64) string {
	t.Helper()

	instanceCounter++
	id := fmt.Sprintf("%032x", instanceCounter)

	scopeKey := "shared"
	if owner != "" {
		scopeKey = "user:" + owner
	}

	_, err := db.New(f.pool).InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: id, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: string(template.UID),
		Generation: generation, Scope: template.Spec.Scope, ScopeKey: scopeKey, OwnerPrincipal: owner,
		Namespace: "instance-" + id[24:],
	})
	require.NoError(t, err)

	return id
}

func TestCatalogViewServersMatchListServersForAUser(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addTemplate("by-subject", "per-user", v1alpha1.Access{Users: []string{"alice"}, Environments: "any"})
	fixture.addTemplate("attested-only", "per-user", v1alpha1.Access{Users: []string{"alice"}, Environments: "attested"})
	fixture.addTemplate("other-user", "per-user", v1alpha1.Access{Users: []string{"bob"}, Environments: "any"})

	view := gateway.NewCatalogView(fixture.pool, fixture.catalog, &policy.Engine{Rules: fixture.rules})

	servers, err := view.Servers(t.Context(), auth.Identity{Principal: "alice", Attestation: auth.AttestationUser})
	require.NoError(t, err)

	assert.ElementsMatch(t, []gateway.ServerEntry{
		{Name: "by-subject", Description: "The by-subject server.", Scope: "per-user", Status: "stopped"},
		{
			Name: "attested-only", Description: "The attested-only server.", Scope: "per-user", Status: "blocked",
			Reason: "Server \"attested-only\" can be used only from an attested environment, not from a signed-in user's machine.",
		},
	}, servers)

	assert.ElementsMatch(t, listServers(t, fixture.connect(t, user{claims: alice().claims, session: "session-1"})), servers,
		"the same entries as toolgate_list_servers, which shows the identity's servers with nothing enabled")
}

func TestCatalogViewLiveInstances(t *testing.T) {
	fixture := newMCPFixture(t)
	perUser := fixture.addTemplate("per-user", "per-user", v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"})
	shared := fixture.addTemplate("shared", "shared", v1alpha1.Access{Users: []string{"alice"}, Environments: "any"})

	current := fixture.insertOwnedInstance(t, perUser, "alice", perUser.Generation)
	sharedID := fixture.insertOwnedInstance(t, shared, "", shared.Generation-1)
	fixture.insertOwnedInstance(t, perUser, "bob", perUser.Generation)

	stopped := fixture.insertOwnedInstance(t, shared, "alice", shared.Generation)
	_, err := db.New(fixture.pool).SetInstanceDesiredStopped(t.Context(), stopped)
	require.NoError(t, err)

	view := gateway.NewCatalogView(fixture.pool, fixture.catalog, &policy.Engine{Rules: fixture.rules})

	ids := func(instances []gateway.InstanceEntry) []string {
		var result []string
		for _, instance := range instances {
			result = append(result, instance.ID)
		}

		return result
	}

	own, err := view.LiveInstances(t.Context(), "alice", false)
	require.NoError(t, err)
	assert.Equal(t, []string{current}, ids(own), "a user sees only their own live instances")
	assert.False(t, own[0].Outdated, "started from the current generation")
	assert.Equal(t, "starting", own[0].Status)

	all, err := view.LiveInstances(t.Context(), "alice", true)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{current, sharedID}, ids(all), "an admin also sees shared instances")

	for _, instance := range all {
		if instance.ID == sharedID {
			assert.True(t, instance.Outdated, "started from an older generation")
		}
	}
}
