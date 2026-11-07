package policy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// A tool offered under an exact name, as the built-in server's are, is
// refused by a rule or temporary block naming that name, as by one naming
// <server>__<tool>.
func TestRulesMatchTheExactProxiedName(t *testing.T) {
	access := everyone()
	access.Environments = policy.EnvironmentAny
	builtin := template("toolgate", access, nil)

	for _, test := range []struct {
		name    string
		glob    string
		refused bool
	}{
		{name: "exact name", glob: "toolgate_allow", refused: true},
		{name: "server and upstream name", glob: "toolgate__allow", refused: true},
		{name: "glob over exact names", glob: "toolgate_*", refused: true},
		{name: "another exact name", glob: "toolgate_deny"},
	} {
		request := policy.Request{
			Identity: laptopUser(), Template: builtin,
			Tool: "allow", ExposedTool: "allow", ProxiedTool: "toolgate_allow",
		}

		t.Run("rule: "+test.name, func(t *testing.T) {
			decision := engine(t, rule("r", v1alpha1.RuleMatch{Tools: []string{test.glob}}, nil)).Decide(request)

			assert.Equal(t, !test.refused, decision.Allowed)

			if test.refused {
				assert.Equal(t, policy.StepRule, decision.Step)
				assert.Equal(t, "rules/r", decision.Rule)
			}
		})

		t.Run("temporary block: "+test.name, func(t *testing.T) {
			blocked := request
			blocked.State = policy.State{Blocks: []*policy.TemporaryBlock{block(t, "b0", v1alpha1.RuleMatch{Tools: []string{test.glob}})}}

			decision := engine(t).Decide(blocked)

			assert.Equal(t, !test.refused, decision.Allowed)

			if test.refused {
				assert.Equal(t, policy.StepTemporaryBlock, decision.Step)
				assert.Equal(t, "b0", decision.Block)
			}
		})
	}
}
