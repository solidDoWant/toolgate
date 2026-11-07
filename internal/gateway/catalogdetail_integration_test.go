//go:build integration

package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

func TestCatalogViewCatalogDetails(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addTemplate("eng-only", "per-session", v1alpha1.Access{Groups: []string{"eng"}, Environments: "any"})
	fixture.addTemplate("ops-only", "shared", v1alpha1.Access{Groups: []string{"ops"}, Environments: "any"})

	remote := fixture.addTemplate("mail", "per-user", v1alpha1.Access{Groups: []string{"eng"}, Environments: "opt-in"})
	remote.Spec.Remote = &v1alpha1.RemoteServer{
		URL:     "https://mail.example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${secret:mail-archiver-api-key}"},
	}
	remote.Spec.DeclaredTools = []v1alpha1.DeclaredTool{{Name: "read"}, {Name: "delete"}, {Name: "send"}}
	remote.Spec.Tools = map[string]v1alpha1.ToolOverride{"delete": {Blocked: true}, "send": {Approval: "required", Environments: "attested"}}
	fixture.catalog.Set(remote, nil)

	view := gateway.NewCatalogView(fixture.pool, fixture.catalog, &policy.Engine{Rules: fixture.rules})
	identity := auth.Identity{Principal: "alice", Groups: []string{"eng"}, Kind: auth.KindUser, Attestation: auth.AttestationUser}

	names := func(infos []gateway.ServerInfo) []string {
		result := []string{}
		for _, info := range infos {
			result = append(result, info.Name)
		}

		return result
	}

	t.Run("a user without a server's group never sees it", func(t *testing.T) {
		infos, err := view.Catalog(t.Context(), identity)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"eng-only", "mail"}, names(infos))

		_, _, found, err := view.ServerDetail(t.Context(), identity, "ops-only")
		require.NoError(t, err)
		assert.False(t, found, "the detail of a server the user can't use looks like a missing one")
	})

	t.Run("scope, environments and kind", func(t *testing.T) {
		infos, err := view.Catalog(t.Context(), identity)
		require.NoError(t, err)

		byName := map[string]gateway.ServerInfo{}
		for _, info := range infos {
			byName[info.Name] = info
		}

		assert.Equal(t, "per-session", byName["eng-only"].Scope)
		assert.Equal(t, "any", byName["eng-only"].Environments)
		assert.False(t, byName["eng-only"].Remote)
		assert.Equal(t, "per-user", byName["mail"].Scope)
		assert.Equal(t, "opt-in", byName["mail"].Environments)
		assert.True(t, byName["mail"].Remote)
		assert.Equal(t, 3, byName["mail"].ToolCount)
		assert.Equal(t, -1, byName["eng-only"].ToolCount, "no declared or learned list")
	})

	t.Run("a secret shows as set or not set, never its value", func(t *testing.T) {
		infos, err := view.Catalog(t.Context(), identity)
		require.NoError(t, err)

		for _, info := range infos {
			if info.Name == "mail" {
				assert.Equal(t, []gateway.SecretStatus{{Name: "mail-archiver-api-key", Set: false}}, info.Secrets)
			}
		}

		_, err = db.New(fixture.pool).PutUserSecret(t.Context(), db.PutUserSecretParams{Principal: "alice", Name: "mail-archiver-api-key", Ciphertext: []byte("sealed-value"), KeyID: 1})
		require.NoError(t, err)

		infos, err = view.Catalog(t.Context(), identity)
		require.NoError(t, err)

		for _, info := range infos {
			if info.Name == "mail" {
				assert.Equal(t, []gateway.SecretStatus{{Name: "mail-archiver-api-key", Set: true}}, info.Secrets)
			}
		}

		other := identity
		other.Principal = "carol"
		infos, err = view.Catalog(t.Context(), other)
		require.NoError(t, err)

		for _, info := range infos {
			if info.Name == "mail" {
				assert.False(t, info.Secrets[0].Set, "another user's secret doesn't count")
			}
		}
	})

	t.Run("the detail marks blocked and approval-required tools", func(t *testing.T) {
		_, tools, found, err := view.ServerDetail(t.Context(), identity, "mail")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, []gateway.ToolInfo{
			{Name: "delete", Blocked: true},
			{Name: "read"},
			{Name: "send", ApprovalRequired: true, Environments: "attested"},
		}, tools)
	})
}
