package policy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
)

func TestRuleReconciler(t *testing.T) {
	valid := rule("block-ci", v1alpha1.RuleMatch{Workspaces: []string{"k8s/ci/*"}}, nil)
	invalid := rule("broken", v1alpha1.RuleMatch{Tools: []string{"github__[delete"}}, nil)
	unwatched := rule("elsewhere", v1alpha1.RuleMatch{}, nil)
	unwatched.Namespace = "other"

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(valid, invalid, unwatched).
		WithStatusSubresource(&v1alpha1.ToolgateRule{}).
		Build()

	rules := policy.NewRules()
	reconciler := &policy.RuleReconciler{Client: kube, Rules: rules, Namespaces: []string{"rules"}}
	policyEngine := &policy.Engine{Rules: rules, Now: func() time.Time { return now }}
	github := template("github", v1alpha1.Access{ServiceAccounts: []string{"system:serviceaccount:ci:runner"}, Environments: "any"}, nil)

	reconcileRule := func(object client.Object) {
		t.Helper()

		_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace(), Name: object.GetName()}})
		require.NoError(t, err)
	}

	condition := func(object client.Object) *metav1.Condition {
		t.Helper()

		var stored v1alpha1.ToolgateRule
		require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(object), &stored))

		return meta.FindStatusCondition(stored.Status.Conditions, policy.ConditionValid)
	}

	reconcileRule(unwatched)
	reconcileRule(invalid)
	assert.Nil(t, condition(unwatched))
	assert.True(t, policyEngine.Decide(policy.Request{Identity: pod("ci"), Template: github}).Allowed, "unwatched and invalid rules don't apply")

	invalidCondition := condition(invalid)
	require.NotNil(t, invalidCondition)
	assert.Equal(t, metav1.ConditionFalse, invalidCondition.Status)
	assert.Contains(t, invalidCondition.Message, "match.tools[0]")

	reconcileRule(valid)

	validCondition := condition(valid)
	require.NotNil(t, validCondition)
	assert.Equal(t, metav1.ConditionTrue, validCondition.Status)
	assert.Equal(t, "rules/block-ci", policyEngine.Decide(policy.Request{Identity: pod("ci"), Template: github}).Rule)

	require.NoError(t, kube.Delete(t.Context(), valid))
	reconcileRule(valid)
	assert.True(t, policyEngine.Decide(policy.Request{Identity: pod("ci"), Template: github}).Allowed, "a deleted rule stops applying")
}
