//go:build integration

package runtime_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

func TestBughuntSharedNamespaceSecretSurvivesTeardown(t *testing.T) {
	fixture := newStopFixture(t)
	everyone := v1alpha1.Access{Groups: []string{"staff"}}

	template := fixture.template("shared-secret", "shared", 0, everyone)
	instance := fixture.sharedInstance(t, template, "failed")

	labels := map[string]string{runtime.LabelInstance: instance.ID, runtime.LabelManagedBy: runtime.ManagedByValue}
	require.NoError(t, fixture.kube.Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: sharedNamespace, Name: "creds-" + instance.ID, Labels: labels,
	}, StringData: map[string]string{"token": "x"}}))

	fixture.pass(t)
	fixture.pass(t)
	fixture.pass(t)
	require.Equal(t, "stopped", fixture.get(t, instance.ID).Status)

	var secret corev1.Secret
	err := fixture.kube.Get(t.Context(), types.NamespacedName{Namespace: sharedNamespace, Name: "creds-" + instance.ID}, &secret)
	assert.Error(t, err, "the stopped instance's Secret is still in the shared namespace")
	if err == nil {
		t.Logf("leaked: Secret %s/%s deletionTimestamp=%v", secret.Namespace, secret.Name, secret.DeletionTimestamp)
	}
}
