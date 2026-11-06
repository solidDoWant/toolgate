package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
)

func TestBuiltinServerAccess(t *testing.T) {
	engine := &policy.Engine{}
	template := gateway.BuiltinTemplate()

	for _, test := range []struct {
		name     string
		identity auth.Identity
		allowed  bool
	}{
		{name: "a signed-in user", identity: auth.Identity{Kind: auth.KindUser, Principal: "alice", Attestation: auth.AttestationUser}, allowed: true},
		{name: "a user in a verified workspace", identity: auth.Identity{Kind: auth.KindUser, Principal: "alice", Attestation: auth.AttestationAttested}, allowed: true},
		{name: "row 209: a service account", identity: auth.Identity{Kind: auth.KindServiceAccount, Principal: "system:serviceaccount:ci:bot", Attestation: auth.AttestationAttested}},
		{name: "row 209: a SPIFFE workload", identity: auth.Identity{Kind: auth.KindSPIFFE, Principal: "spiffe://example.com/w", Attestation: auth.AttestationAttested}},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := engine.Decide(policy.Request{Identity: test.identity, Template: template, Tool: "list_scopes"})

			assert.Equal(t, test.allowed, decision.Allowed)

			if !test.allowed {
				assert.Equal(t, policy.StepAccess, decision.Step)
			}
		})
	}
}

func TestBuiltinServerInCatalog(t *testing.T) {
	t.Run("row 210: a catalog without the built-in server doesn't offer it", func(t *testing.T) {
		templates := catalog.New()

		_, err := templates.Offered(gateway.BuiltinServerName)
		require.ErrorIs(t, err, catalog.ErrNotFound)
		assert.Empty(t, templates.List())
		assert.Empty(t, templates.LastValidByUID())
	})

	t.Run("a catalog with the built-in server offers, lists and runs it under its own key", func(t *testing.T) {
		templates := catalog.New()
		builtin := gateway.BuiltinTemplate()
		templates.SetBuiltin(builtin)

		offered, err := templates.Offered(gateway.BuiltinServerName)
		require.NoError(t, err)
		assert.Equal(t, builtin, offered)
		assert.Equal(t, "shared", offered.Spec.Scope)
		assert.Equal(t, policy.EnvironmentAny, offered.Spec.Access.Environments)

		require.Len(t, templates.List(), 1)
		assert.Contains(t, templates.LastValidByUID(), string(builtin.UID))
		assert.True(t, templates.UIDs()[string(builtin.UID)])

		_, running := templates.Running(types.NamespacedName{Namespace: builtin.Namespace, Name: builtin.Name})
		assert.True(t, running)
	})
}
