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
)

func TestCatalogViewAccessPatterns(t *testing.T) {
	fixture := newMCPFixture(t)
	fixture.addTemplate("public", "shared", v1alpha1.Access{Users: []string{"*"}, Environments: "any"})
	fixture.addTemplate("company", "shared", v1alpha1.Access{Users: []string{"bob", "*@example.com"}, Groups: []string{"eng", "dev-*"}, Environments: "any"})
	fixture.addTemplate("private", "shared", v1alpha1.Access{Groups: []string{"ops"}, Environments: "any"})

	view := gateway.NewCatalogView(fixture.pool, fixture.catalog, &policy.Engine{Rules: fixture.rules})

	t.Run("a users star server is listed for a user in no groups", func(t *testing.T) {
		infos, err := view.Catalog(t.Context(), auth.Identity{Principal: "nobody", Kind: auth.KindUser, Attestation: auth.AttestationUser})
		require.NoError(t, err)
		require.Len(t, infos, 1)
		assert.Equal(t, "public", infos[0].Name)
		assert.True(t, infos[0].Everyone)
	})

	t.Run("patterns are shown as written and exact names are not", func(t *testing.T) {
		identity := auth.Identity{Principal: "carol", Email: "carol@example.com", Kind: auth.KindUser, Attestation: auth.AttestationUser}

		info, _, found, err := view.ServerDetail(t.Context(), identity, "company")
		require.NoError(t, err)
		require.True(t, found)
		assert.False(t, info.Everyone)
		assert.Equal(t, []string{"*@example.com"}, info.UserPatterns)
		assert.Equal(t, []string{"dev-*"}, info.GroupPatterns)
	})
}
