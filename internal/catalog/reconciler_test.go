package catalog_test

import (
	"testing"

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
	"github.com/solidDoWant/toolgate/internal/catalog"
)

type reconcilerFixture struct {
	client     client.Client
	catalog    *catalog.Catalog
	reconciler *catalog.Reconciler
}

func newReconcilerFixture(t *testing.T, objects ...client.Object) reconcilerFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.MCPServerTemplate{}).
		Build()

	templates := catalog.New()

	return reconcilerFixture{
		client:  kube,
		catalog: templates,
		reconciler: &catalog.Reconciler{
			Client:     kube,
			Catalog:    templates,
			Namespaces: []string{"catalog", "catalog-b"},
		},
	}
}

func (f reconcilerFixture) reconcile(t *testing.T, key types.NamespacedName) {
	t.Helper()

	_, err := f.reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
}

func (f reconcilerFixture) condition(t *testing.T, key types.NamespacedName) *metav1.Condition {
	t.Helper()

	var template v1alpha1.MCPServerTemplate
	require.NoError(t, f.client.Get(t.Context(), key, &template))

	return meta.FindStatusCondition(template.Status.Conditions, catalog.ConditionValid)
}

func keyOf(template *v1alpha1.MCPServerTemplate) types.NamespacedName {
	return types.NamespacedName{Namespace: template.Namespace, Name: template.Name}
}

func TestReconcilerReportsInvalidTemplates(t *testing.T) {
	tests := []struct {
		name     string
		template *v1alpha1.MCPServerTemplate
		reason   string
	}{
		{
			name:     "[AC-1-23] secret outside a Secret",
			template: newTemplate("github", catalog.ScopePerSession, podWithEnv("${secret:github-token}")),
			reason:   "${secret:github-token} is allowed only in the data or stringData values of a Secret",
		},
		{
			name:     "[AC-1-24] cluster-scoped kind",
			template: newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"n"}}`),
			reason:   "v1 Namespace is not an allowed kind",
		},
		{
			name:     "[AC-1-24] RBAC kind",
			template: newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"n"}}`),
			reason:   "Role is an RBAC object",
		},
		{
			name:     "[AC-1-14] shared template using a principal",
			template: newTemplate("github", catalog.ScopeShared, podWithEnv("${principal.id}")),
			reason:   "a shared template may not use ${principal.id}",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReconcilerFixture(t, test.template)
			key := keyOf(test.template)

			fixture.reconcile(t, key)

			condition := fixture.condition(t, key)
			require.NotNil(t, condition)
			assert.Equal(t, metav1.ConditionFalse, condition.Status)
			assert.Equal(t, catalog.ReasonInvalid, condition.Reason)
			assert.Contains(t, condition.Message, test.reason)
			assert.Equal(t, test.template.Generation, condition.ObservedGeneration)

			_, err := fixture.catalog.Offered("github")

			var invalid *catalog.InvalidError
			require.ErrorAs(t, err, &invalid)
			assert.Contains(t, invalid.Reason, test.reason)
			assert.Empty(t, fixture.catalog.List())
		})
	}
}

func TestReconcilerOffersValidTemplates(t *testing.T) {
	template := newTemplate("github", catalog.ScopePerUser, serviceResource, secretWith("stringData", "${secret:token}"))
	fixture := newReconcilerFixture(t, template)
	key := keyOf(template)

	fixture.reconcile(t, key)

	condition := fixture.condition(t, key)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)

	offered, err := fixture.catalog.Offered("github")
	require.NoError(t, err)
	assert.Equal(t, template.Spec, offered.Spec)
	require.Len(t, fixture.catalog.List(), 1)

	// Only the status is written: the spec in the cluster is unchanged.
	var stored v1alpha1.MCPServerTemplate
	require.NoError(t, fixture.client.Get(t.Context(), key, &stored))
	assert.Equal(t, template.Spec, stored.Spec)
}

func TestReconcilerIgnoresUnwatchedNamespaces(t *testing.T) {
	template := newTemplate("github", catalog.ScopePerUser)
	template.Namespace = "elsewhere"
	fixture := newReconcilerFixture(t, template)

	fixture.reconcile(t, keyOf(template))

	_, err := fixture.catalog.Offered("github")
	require.ErrorIs(t, err, catalog.ErrNotFound)
	assert.Nil(t, fixture.condition(t, keyOf(template)))
}

// A valid template edited into an invalid one isn't offered, new enables get
// the validation reason, and running instances keep the last valid
// generation (docs/design/phase-1.md §4, "Template becomes invalid").
func TestReconcilerKeepsLastValidGenerationForRunningInstances(t *testing.T) {
	template := newTemplate("github", catalog.ScopePerUser, serviceResource)
	template.Spec.Description = "generation 1"
	fixture := newReconcilerFixture(t, template)
	key := keyOf(template)

	fixture.reconcile(t, key)

	running, found := fixture.catalog.Running(key)
	require.True(t, found)
	assert.Equal(t, "generation 1", running.Spec.Description)

	var stored v1alpha1.MCPServerTemplate
	require.NoError(t, fixture.client.Get(t.Context(), key, &stored))
	stored.Spec.Description = "generation 2"
	stored.Spec.Resources = append(stored.Spec.Resources, runtime.RawExtension{Raw: []byte(podWithEnv("${secret:token}"))})
	stored.Generation = 2
	require.NoError(t, fixture.client.Update(t.Context(), &stored))

	fixture.reconcile(t, key)

	condition := fixture.condition(t, key)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, int64(2), condition.ObservedGeneration)

	_, err := fixture.catalog.Offered("github")

	var invalid *catalog.InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Reason, "${secret:token} is allowed only")
	assert.Empty(t, fixture.catalog.List())

	running, found = fixture.catalog.Running(key)
	require.True(t, found)
	assert.Equal(t, "generation 1", running.Spec.Description)

	// A valid replacement becomes the generation running instances see.
	require.NoError(t, fixture.client.Get(t.Context(), key, &stored))
	stored.Spec.Description = "generation 3"
	stored.Spec.Resources = stored.Spec.Resources[:1]
	stored.Generation = 3
	require.NoError(t, fixture.client.Update(t.Context(), &stored))

	fixture.reconcile(t, key)

	running, found = fixture.catalog.Running(key)
	require.True(t, found)
	assert.Equal(t, "generation 3", running.Spec.Description)

	_, err = fixture.catalog.Offered("github")
	require.NoError(t, err)

	// Deleting the template removes it for running instances too.
	require.NoError(t, fixture.client.Delete(t.Context(), &stored))
	fixture.reconcile(t, key)

	_, found = fixture.catalog.Running(key)
	assert.False(t, found)

	_, err = fixture.catalog.Offered("github")
	require.ErrorIs(t, err, catalog.ErrNotFound)
}

func TestReconcilerMarksDuplicateServerNamesInvalid(t *testing.T) {
	first := newTemplate("github", catalog.ScopePerUser, serviceResource)
	second := newTemplate("github", catalog.ScopePerUser, serviceResource)
	second.Namespace = "catalog-b"
	fixture := newReconcilerFixture(t, first, second)

	fixture.reconcile(t, keyOf(first))
	require.Equal(t, metav1.ConditionTrue, fixture.condition(t, keyOf(first)).Status)

	fixture.reconcile(t, keyOf(second))

	for _, key := range []types.NamespacedName{keyOf(first), keyOf(second)} {
		condition := fixture.condition(t, key)
		require.NotNil(t, condition)
		assert.Equal(t, metav1.ConditionFalse, condition.Status, key.String())
		assert.Equal(t, "duplicate server name", condition.Message, key.String())
	}

	_, err := fixture.catalog.Offered("github")

	var invalid *catalog.InvalidError
	require.ErrorAs(t, err, &invalid)

	// The first was valid before, so its running instances keep it.
	_, found := fixture.catalog.Running(keyOf(first))
	assert.True(t, found)

	require.NoError(t, fixture.client.Delete(t.Context(), second))
	fixture.reconcile(t, keyOf(second))

	assert.Equal(t, metav1.ConditionTrue, fixture.condition(t, keyOf(first)).Status)
	_, err = fixture.catalog.Offered("github")
	require.NoError(t, err)
}
