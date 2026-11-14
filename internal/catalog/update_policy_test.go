package catalog_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
)

func TestValidateUpdatePolicy(t *testing.T) {
	idle := func(duration time.Duration) *metav1.Duration { return &metav1.Duration{Duration: duration} }

	tests := []struct {
		name    string
		policy  *v1alpha1.UpdatePolicy
		problem string
	}{
		{name: "unset"},
		{name: "manual", policy: &v1alpha1.UpdatePolicy{Mode: "manual"}},
		{name: "immediate", policy: &v1alpha1.UpdatePolicy{Mode: "immediate"}},
		{name: "whenIdle at the minimum", policy: &v1alpha1.UpdatePolicy{Mode: "whenIdle", IdleFor: idle(time.Minute)}},
		{name: "whenIdle under the minimum", policy: &v1alpha1.UpdatePolicy{Mode: "whenIdle", IdleFor: idle(time.Minute - time.Second)}, problem: "at least 1m0s"},
		{name: "whenIdle without idleFor", policy: &v1alpha1.UpdatePolicy{Mode: "whenIdle"}, problem: "at least 1m0s"},
		{name: "immediate with idleFor", policy: &v1alpha1.UpdatePolicy{Mode: "immediate", IdleFor: idle(time.Hour)}, problem: "only with mode whenIdle"},
		{name: "unknown mode", policy: &v1alpha1.UpdatePolicy{Mode: "sometimes"}, problem: "must be one of"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := &v1alpha1.MCPServerTemplate{}
			template.Name = "update-policy"
			template.Spec.UpdatePolicy = test.policy

			problems := catalog.Reason(catalog.Validator{}.Validate(template))
			if test.problem == "" {
				assert.NotContains(t, problems, "updatePolicy")

				return
			}

			assert.Contains(t, problems, test.problem)
		})
	}
}
