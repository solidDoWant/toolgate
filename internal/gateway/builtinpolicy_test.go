package gateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// The policy request of a built-in tool carries its exact name, so a rule
// naming toolgate_allow refuses it, as one naming toolgate__allow does.
func TestBuiltinToolPolicyRequestMatchesExactNames(t *testing.T) {
	current := requestContext{identity: auth.Identity{
		Kind: auth.KindUser, Principal: "alice", Session: "s1",
		Workspace: "claimed/default", Attestation: auth.AttestationUser,
	}}

	for _, tool := range []string{builtinListScopes, builtinAllow, builtinDeny} {
		for _, glob := range []string{"toolgate_" + tool, "toolgate__" + tool} {
			t.Run(glob, func(t *testing.T) {
				set := policy.NewRules()
				require.NoError(t, set.Set(&v1alpha1.ToolgateRule{
					ObjectMeta: metav1.ObjectMeta{Namespace: "rules", Name: "r"},
					Spec:       v1alpha1.ToolgateRuleSpec{Match: v1alpha1.RuleMatch{Tools: []string{glob}}, Reason: "no"},
				}))
				engine := &policy.Engine{Rules: set, Now: time.Now}

				decision := engine.Decide((&metaTools{}).policyRequest(current, BuiltinTemplate(), tool, policy.State{}))

				assert.False(t, decision.Allowed)
				assert.Equal(t, policy.StepRule, decision.Step)
			})
		}
	}

	t.Run("another server's tool of the same name isn't offered under the exact name", func(t *testing.T) {
		other := BuiltinTemplate().DeepCopy()
		other.Name = "github"

		request := (&metaTools{}).policyRequest(current, other, builtinAllow, policy.State{})

		assert.Empty(t, request.ProxiedTool)
	})
}
