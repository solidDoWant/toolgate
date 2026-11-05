//go:build integration

package testutil_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// mcpServerTemplate returns an MCPServerTemplate for server-side apply.
func mcpServerTemplate(name, environments string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "MCPServerTemplate",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"scope":    "per-session",
			"access":   map[string]any{"environments": environments},
			"endpoint": map[string]any{"service": "server", "port": int64(8080)},
		},
	}}
}

func TestStartEnvtest(t *testing.T) {
	env := testutil.StartEnvtest(t)
	ctx := t.Context()

	t.Run("server-side apply with the toolgate field manager", func(t *testing.T) {
		require.NoError(t, env.Client.Apply(ctx,
			client.ApplyConfigurationFromUnstructured(mcpServerTemplate("applied", "attested")),
			client.FieldOwner("toolgate"), client.ForceOwnership))

		var stored v1alpha1.MCPServerTemplate
		require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "applied"}, &stored))
		assert.Equal(t, "attested", stored.Spec.Access.Environments)

		var managers []string

		for _, entry := range stored.ManagedFields {
			if entry.Operation == metav1.ManagedFieldsOperationApply {
				managers = append(managers, entry.Manager)
			}
		}

		assert.Equal(t, []string{"toolgate"}, managers)
	})

	t.Run("status update through the status subresource", func(t *testing.T) {
		var stored v1alpha1.MCPServerTemplate
		require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "applied"}, &stored))

		meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "Valid", Message: "the template is valid",
		})
		stored.Spec.Description = "changed through the status subresource"
		require.NoError(t, env.Client.Status().Update(ctx, &stored))

		var reread v1alpha1.MCPServerTemplate
		require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "applied"}, &reread))
		assert.True(t, meta.IsStatusConditionTrue(reread.Status.Conditions, "Ready"))
		assert.Empty(t, reread.Spec.Description, "a status update must not change the spec")
	})

	t.Run("CEL validation rejects an unknown environments value", func(t *testing.T) {
		err := env.Client.Apply(ctx,
			client.ApplyConfigurationFromUnstructured(mcpServerTemplate("rejected", "sometimes")),
			client.FieldOwner("toolgate"), client.ForceOwnership)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid error, got %v", err)
		assert.ErrorContains(t, err, "environments must be one of any, opt-in, attested")
	})
}
