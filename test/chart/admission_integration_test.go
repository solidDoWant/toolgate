//go:build integration

package chart

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

// chartAccessKinds are the rendered objects that decide what the gateway's
// service account may write: its RBAC and the admission policy.
var chartAccessKinds = map[string]bool{
	"ClusterRole": true, "ClusterRoleBinding": true, "Role": true, "RoleBinding": true,
	"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true,
}

func testDeployment(name string) *appsv1.Deployment {
	labels := map[string]string{"app": name}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: releaseNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "ghcr.io/example/server:1"}}},
			},
		},
	}
}

func refusedByThePolicy(t require.TestingT, err error, _ ...any) {
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err) || apierrors.IsInvalid(err), "refused, got %v", err)
	assert.Contains(t, err.Error(), "toolgate's service account may write only in namespaces labelled")
}

func TestGatewayMayScaleOnlyTheEmbeddingsServer(t *testing.T) {
	manifests := renderQuickStart(t, scaleToZeroValues...)
	cluster := testutil.StartEnvtest(t)
	ctx := t.Context()

	admin, err := kubernetes.NewForConfig(cluster.Config)
	require.NoError(t, err)

	_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: releaseNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)

	for _, item := range manifests {
		if !chartAccessKinds[item.Kind] {
			continue
		}

		object := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal(item.raw, &object.Object))
		require.NoError(t, cluster.Client.Create(ctx, object), "create %s %s", item.Kind, item.Metadata.Name)
	}

	const otherDeployment = releaseName + "-other"

	for _, name := range []string{embeddingsDeployment, otherDeployment} {
		_, err := admin.AppsV1().Deployments(releaseNamespace).Create(ctx, testDeployment(name), metav1.CreateOptions{})
		require.NoError(t, err)
	}

	gatewayConfig := rest.CopyConfig(cluster.Config)
	gatewayConfig.Impersonate = rest.ImpersonationConfig{
		UserName: serviceAccount,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + releaseNamespace, "system:authenticated"},
	}
	gateway, err := kubernetes.NewForConfig(gatewayConfig)
	require.NoError(t, err)

	// The API server loads a new policy in the background. The chart's
	// ClusterRole lets the gateway create ConfigMaps anywhere, so once such a
	// write in the release namespace is refused, the policy is in force.
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{GenerateName: "probe-", Namespace: releaseNamespace}}
		_, err := gateway.CoreV1().ConfigMaps(releaseNamespace).Create(ctx, configMap, metav1.CreateOptions{})
		refusedByThePolicy(collect, err)
	}, 30*time.Second, 200*time.Millisecond, "the admission policy is in force")

	scaleTo := func(name string, replicas int) error {
		patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
		_, err := gateway.AppsV1().Deployments(releaseNamespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}, "scale")

		return err
	}

	t.Run("the scale of the embeddings server is accepted", func(t *testing.T) {
		require.NoError(t, scaleTo(embeddingsDeployment, 0))

		scale, err := gateway.AppsV1().Deployments(releaseNamespace).GetScale(ctx, embeddingsDeployment, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, int32(0), scale.Spec.Replicas)

		require.NoError(t, scaleTo(embeddingsDeployment, 1))

		_, err = gateway.AppsV1().Deployments(releaseNamespace).Get(ctx, embeddingsDeployment, metav1.GetOptions{})
		require.NoError(t, err)
	})

	t.Run("a spec patch of the embeddings server is refused", func(t *testing.T) {
		// The chart's ClusterRole allows patching Deployments, so only the
		// admission policy stands in the way.
		_, err := gateway.AppsV1().Deployments(releaseNamespace).Patch(ctx, embeddingsDeployment, types.MergePatchType,
			[]byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{})
		refusedByThePolicy(t, err)
	})

	t.Run("the scale of another Deployment is refused by RBAC", func(t *testing.T) {
		err := scaleTo(otherDeployment, 0)
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err), "refused, got %v", err)
	})

	t.Run("the scale of another Deployment is refused by the admission policy", func(t *testing.T) {
		// Give the gateway every right on Deployments in the release namespace, so the
		// refusal comes from the admission policy, not RBAC.
		// envtest runs no controller manager, so the built-in admin role
		// aggregates nothing; this Role grants the rights directly.
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "test-gateway-admin", Namespace: releaseNamespace},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments", "deployments/scale"}, Verbs: []string{"*"}}},
		}
		_, err := admin.RbacV1().Roles(releaseNamespace).Create(ctx, role, metav1.CreateOptions{})
		require.NoError(t, err)

		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "test-gateway-admin", Namespace: releaseNamespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "test-gateway-admin"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: releaseName, Namespace: releaseNamespace}},
		}
		_, err = admin.RbacV1().RoleBindings(releaseNamespace).Create(ctx, binding, metav1.CreateOptions{})
		require.NoError(t, err)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			refusedByThePolicy(collect, scaleTo(otherDeployment, 0))
		}, 30*time.Second, 200*time.Millisecond)

		deployment, err := admin.AppsV1().Deployments(releaseNamespace).Get(ctx, otherDeployment, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, int32(1), *deployment.Spec.Replicas)
	})
}
