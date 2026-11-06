package policy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// attestedUser is a user whose request carried a workload token bound to
// them: still a user, with a verified workspace.
func attestedUser() auth.Identity {
	identity := laptopUser()
	identity.Workspace = "coder/alice/dev"
	identity.Attestation = auth.AttestationAttested

	return identity
}

func TestAttestedUserEnvironments(t *testing.T) {
	access := func(environments string) v1alpha1.Access {
		return v1alpha1.Access{Users: []string{"alice-sub"}, Environments: environments}
	}

	for _, environments := range []string{"attested", "opt-in", ""} {
		decision := engine(t).Decide(policy.Request{Identity: attestedUser(), Template: template("github", access(environments), nil)})
		assert.True(t, decision.Allowed, "environments %q: %s", environments, decision.Reason)
	}

	// The access list is the user's: an attested user isn't a workload.
	decision := engine(t).Decide(policy.Request{
		Identity: attestedUser(),
		Template: template("github", v1alpha1.Access{ServiceAccounts: []string{"system:serviceaccount:coder:workspace"}, Environments: "any"}, nil),
	})
	assert.False(t, decision.Allowed)
}

func TestAttestedUserMatchesAttestedRules(t *testing.T) {
	rules := engine(t, rule("no-attested", v1alpha1.RuleMatch{Attestation: "attested"}, nil))
	access := v1alpha1.Access{Users: []string{"alice-sub"}, Environments: "any"}

	decision := rules.Decide(policy.Request{Identity: attestedUser(), Template: template("github", access, nil)})
	assert.False(t, decision.Allowed)
	assert.Equal(t, policy.StepRule, decision.Step)

	decision = rules.Decide(policy.Request{Identity: laptopUser(), Template: template("github", access, nil)})
	assert.True(t, decision.Allowed, decision.Reason)
}
