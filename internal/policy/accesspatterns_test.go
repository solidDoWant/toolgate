package policy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
)

func TestAccessPatterns(t *testing.T) {
	unverified := laptopUser()
	unverified.Email = ""

	noGroups := laptopUser()
	noGroups.Groups = nil

	literalStar := laptopUser()
	literalStar.Principal = "*"

	tests := []struct {
		name     string
		identity auth.Identity
		access   v1alpha1.Access
		allowed  bool
	}{
		{name: "star matches a user in no groups", identity: noGroups, access: v1alpha1.Access{Users: []string{"*"}}, allowed: true},
		{name: "star doesn't match a service account", identity: pod("ci"), access: v1alpha1.Access{Users: []string{"*"}}},
		{name: "star doesn't match a SPIFFE workload", identity: spiffeWorkload(), access: v1alpha1.Access{Users: []string{"*"}}},
		{name: "star doesn't match an identity without a kind", identity: auth.Identity{Principal: "alice-sub"}, access: v1alpha1.Access{Users: []string{"*"}}},
		{name: "star groups don't match a service account", identity: pod("ci"), access: v1alpha1.Access{Groups: []string{"*"}}},
		{name: "subject pattern", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"alice-*"}}, allowed: true},
		{name: "subject pattern is anchored", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"alice"}}},
		{name: "subject pattern is anchored at the end", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"lice-*"}}},
		{name: "verified email domain", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"*@example.com"}}, allowed: true},
		{name: "unverified email doesn't match a domain pattern", identity: unverified, access: v1alpha1.Access{Users: []string{"*@example.com"}}},
		{name: "other email domain", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"*@example.org"}}},
		{name: "domain pattern doesn't match a subdomain trick", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"*@example.co"}}},
		{name: "question mark matches one character", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"alice-su?"}}, allowed: true},
		{name: "group prefix", identity: laptopUser(), access: v1alpha1.Access{Groups: []string{"de*"}}, allowed: true},
		{name: "other group prefix", identity: laptopUser(), access: v1alpha1.Access{Groups: []string{"ops-*"}}},
		{name: "star groups match a user with a group", identity: laptopUser(), access: v1alpha1.Access{Groups: []string{"*"}}, allowed: true},
		{name: "star groups don't match a user in no groups", identity: noGroups, access: v1alpha1.Access{Groups: []string{"*"}}},
		{name: "exact entry still matches exactly", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"alice-sub"}}, allowed: true},
		{name: "escaped star matches only a literal star", identity: literalStar, access: v1alpha1.Access{Users: []string{`\*`}}, allowed: true},
		{name: "escaped star doesn't match other subjects", identity: laptopUser(), access: v1alpha1.Access{Users: []string{`\*`}}},
		{name: "invalid pattern matches nothing", identity: laptopUser(), access: v1alpha1.Access{Users: []string{"[*"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.allowed, policy.HasAccess(test.identity, test.access))
		})
	}
}

func TestEveryoneServerKeepsOtherChecks(t *testing.T) {
	everyone := func(environments string) v1alpha1.Access {
		return v1alpha1.Access{Users: []string{"*"}, Environments: environments}
	}
	stranger := auth.Identity{Kind: auth.KindUser, Principal: "stranger", Attestation: auth.AttestationUser}

	t.Run("is allowed with environments any", func(t *testing.T) {
		decision := engine(t).Decide(policy.Request{Identity: stranger, Template: template("kubesearch", everyone("any"), nil)})
		assert.True(t, decision.Allowed, decision.Reason)
	})

	t.Run("is blocked by an unverified default of none", func(t *testing.T) {
		state := policy.State{Scope: policy.EffectiveScope(stranger, policy.ScopeRows{UnverifiedDefault: "none"})}
		decision := engine(t).Decide(policy.Request{Identity: stranger, Template: template("kubesearch", everyone("any"), nil), State: state})
		assert.False(t, decision.Allowed)
		assert.Equal(t, policy.StepWorkspaceScope, decision.Step)
	})

	t.Run("is blocked by a rule", func(t *testing.T) {
		blocking := rule("block", v1alpha1.RuleMatch{Servers: []string{"kubesearch"}}, nil)
		decision := engine(t, blocking).Decide(policy.Request{Identity: stranger, Template: template("kubesearch", everyone("any"), nil)})
		assert.False(t, decision.Allowed)
		assert.Equal(t, policy.StepRule, decision.Step)
	})

	t.Run("is blocked by environments attested without attestation", func(t *testing.T) {
		decision := engine(t).Decide(policy.Request{Identity: stranger, Template: template("kubesearch", everyone("attested"), nil)})
		assert.False(t, decision.Allowed)
		assert.Equal(t, policy.StepEnvironment, decision.Step)
	})
}
