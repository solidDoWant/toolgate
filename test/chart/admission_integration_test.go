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
	"k8s.io/apimachinery/pkg/api/resource"
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

const (
	volumeIDLabel   = "toolgate.soliddowant.dev/volume-id"
	provisionedBy   = "pv.kubernetes.io/provisioned-by"
	volumeClaimUID  = "0b7e1c52-6f43-4c1b-9d0a-1f2e3d4c5b6a"
	instanceClaimNS = "tg-instance"
)

type volumeSpec struct {
	name        string
	labels      map[string]string
	annotations map[string]string
	claimNS     string
	claimUID    string
	reclaim     corev1.PersistentVolumeReclaimPolicy
}

func testVolume(spec volumeSpec) *corev1.PersistentVolume {
	volume := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: spec.name, Labels: spec.labels, Annotations: spec.annotations},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: spec.reclaim,
			PersistentVolumeSource:        corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/" + spec.name}},
		},
	}
	if spec.claimNS != "" {
		volume.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: spec.claimNS, Name: "data", UID: types.UID(spec.claimUID)}
	}

	return volume
}

func refusedByTheVolumeRules(t require.TestingT, err error, _ ...any) {
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err) || apierrors.IsInvalid(err), "refused, got %v", err)
	assert.Contains(t, err.Error(), "toolgate's service account may")
}

func TestGatewayMayChangeOnlyItsOwnVolumes(t *testing.T) {
	manifests := renderQuickStart(t, "--set", "config.persistentVolumes.enabled=true")
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

	gatewayConfig := rest.CopyConfig(cluster.Config)
	gatewayConfig.Impersonate = rest.ImpersonationConfig{
		UserName: serviceAccount,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + releaseNamespace, "system:authenticated"},
	}
	gateway, err := kubernetes.NewForConfig(gatewayConfig)
	require.NoError(t, err)

	// The API server loads a new policy in the background. Once a write in the
	// release namespace is refused, the policy is in force.
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{GenerateName: "probe-", Namespace: releaseNamespace}}
		_, err := gateway.CoreV1().ConfigMaps(releaseNamespace).Create(ctx, configMap, metav1.CreateOptions{})
		refusedByThePolicy(collect, err)
	}, 30*time.Second, 200*time.Millisecond, "the admission policy is in force")

	create := func(t *testing.T, spec volumeSpec) {
		t.Helper()

		_, err := admin.CoreV1().PersistentVolumes().Create(ctx, testVolume(spec), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	patch := func(name, body string) error {
		_, err := gateway.CoreV1().PersistentVolumes().Patch(ctx, name, types.MergePatchType, []byte(body), metav1.PatchOptions{})

		return err
	}
	label := func(name string) error {
		return patch(name, `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"toolgate","`+volumeIDLabel+`":"vol-1"}}}`)
	}
	remove := func(name string) error {
		return gateway.CoreV1().PersistentVolumes().Delete(ctx, name, metav1.DeleteOptions{})
	}
	ownLabels := map[string]string{"app.kubernetes.io/managed-by": "toolgate", volumeIDLabel: "vol-1"}
	provisioned := map[string]string{provisionedBy: "example.com/csi"}

	t.Run("an unlabelled volume can't be patched", func(t *testing.T) {
		create(t, volumeSpec{name: "unlabelled-patch", reclaim: corev1.PersistentVolumeReclaimRetain})
		refusedByTheVolumeRules(t, label("unlabelled-patch"))
	})

	t.Run("an unlabelled volume can't be deleted", func(t *testing.T) {
		create(t, volumeSpec{name: "unlabelled-delete", reclaim: corev1.PersistentVolumeReclaimDelete})
		refusedByTheVolumeRules(t, remove("unlabelled-delete"))
	})

	t.Run("a volume bound in another namespace can't be labelled", func(t *testing.T) {
		create(t, volumeSpec{name: "pvc-" + volumeClaimUID + "-x", annotations: provisioned, claimNS: "default", claimUID: volumeClaimUID})
		refusedByTheVolumeRules(t, label("pvc-"+volumeClaimUID+"-x"))
	})

	t.Run("a labelled volume can't lose its ID", func(t *testing.T) {
		create(t, volumeSpec{name: "keeps-id", labels: ownLabels, claimNS: instanceClaimNS, claimUID: volumeClaimUID})
		refusedByTheVolumeRules(t, patch("keeps-id", `{"metadata":{"labels":{"`+volumeIDLabel+`":null}}}`))
		refusedByTheVolumeRules(t, patch("keeps-id", `{"metadata":{"labels":{"`+volumeIDLabel+`":"vol-2"}}}`))
	})

	t.Run("a labelled volume can't point at a claim outside tg- namespaces", func(t *testing.T) {
		create(t, volumeSpec{name: "keeps-claim", labels: ownLabels, claimNS: instanceClaimNS, claimUID: volumeClaimUID})
		refusedByTheVolumeRules(t, patch("keeps-claim", `{"spec":{"claimRef":{"namespace":"default"}}}`))
		require.NoError(t, patch("keeps-claim", `{"spec":{"claimRef":{"namespace":"tg-other"}}}`))
	})

	t.Run("a Retain volume can't be deleted", func(t *testing.T) {
		create(t, volumeSpec{name: "retained", labels: ownLabels, reclaim: corev1.PersistentVolumeReclaimRetain})
		refusedByTheVolumeRules(t, remove("retained"))

		require.NoError(t, patch("retained", `{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`))
		require.NoError(t, remove("retained"))
	})

	t.Run("a volume bound to an instance claim gets its first label when a provisioner made it", func(t *testing.T) {
		name := "pvc-" + volumeClaimUID
		create(t, volumeSpec{name: name, annotations: provisioned, claimNS: instanceClaimNS, claimUID: volumeClaimUID})
		require.NoError(t, label(name))
	})

	t.Run("a volume whose name is not pvc- and the claim UID can't get its first label", func(t *testing.T) {
		create(t, volumeSpec{name: "static-volume", annotations: provisioned, claimNS: instanceClaimNS, claimUID: volumeClaimUID})
		refusedByTheVolumeRules(t, label("static-volume"))
	})

	t.Run("a volume with no provisioner annotation can't get its first label", func(t *testing.T) {
		const uid = "5d1f0a77-2c3e-4b8a-8e6d-7a9b0c1d2e3f"

		create(t, volumeSpec{name: "pvc-" + uid, claimNS: instanceClaimNS, claimUID: uid})
		refusedByTheVolumeRules(t, label("pvc-"+uid))
	})
}
