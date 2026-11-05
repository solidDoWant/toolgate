//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// hostPod is a pod that would give root on the node: privileged, in the host's
// PID and network namespaces, with the node's root file system mounted.
func hostPod(namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-host", Namespace: namespace},
		Spec: corev1.PodSpec{
			HostPID:     true,
			HostNetwork: true,
			Containers: []corev1.Container{{
				Name:    "host",
				Image:   "busybox",
				Command: []string{"sleep", "3600"},
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr.To(true),
					RunAsUser:  ptr.To[int64](0),
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "host", MountPath: "/host"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "host",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
			}},
		},
	}
}

// hostPathPod is an otherwise restricted pod that mounts the node's root file
// system.
func hostPathPod(namespace string) *corev1.Pod {
	pod := restrictedPod(namespace)
	pod.Name = "e2e-hostpath"
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "host", MountPath: "/host"}}
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "host",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
	}}

	return pod
}

// restrictedPod is a pod that meets Pod Security restricted.
func restrictedPod(namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-restricted", Namespace: namespace},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To[int64](65532),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "main",
				Image:   "busybox",
				Command: []string{"sleep", "3600"},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

// grantNamespaceUpdate lets the gateway's service account update every
// namespace, so a refusal of a namespace update comes from the admission
// policy, not RBAC.
func grantNamespaceUpdate(t *testing.T, admin *kubernetes.Clientset, name string) {
	t.Helper()

	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"update", "patch"}}},
	}
	_, err := admin.RbacV1().ClusterRoles().Create(t.Context(), role, metav1.CreateOptions{})
	require.NoError(t, err)

	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: gatewayServiceAccount, Namespace: releaseNamespace()}},
	}
	_, err = admin.RbacV1().ClusterRoleBindings().Create(t.Context(), binding, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = admin.RbacV1().ClusterRoleBindings().Delete(context.Background(), name, metav1.DeleteOptions{})
		_ = admin.RbacV1().ClusterRoles().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

// TestGatewayCannotEscapePodSecurity reproduces a compromised gateway that
// tries to run a pod with root on the node, and checks that every step is
// refused.
func TestGatewayCannotEscapePodSecurity(t *testing.T) {
	admin := adminClient(t)
	gateway := gatewayClient(t)

	suffix := rand.String(8)

	t.Run("a namespace with only toolgate's label is refused", func(t *testing.T) {
		name := "tg-e2e-escape-" + suffix
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"toolgate.soliddowant.dev/writable": "true"},
		}}

		_, err := gateway.CoreV1().Namespaces().Create(t.Context(), namespace, metav1.CreateOptions{})
		if err == nil {
			_ = admin.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
		}

		refusedWith("pod-security.kubernetes.io/enforce=restricted")(t, err)
	})

	t.Run("a pod in a toolgate namespace without Pod Security restricted is refused", func(t *testing.T) {
		// An admin-made shared namespace that lacks the Pod Security label.
		name := "e2e-unrestricted-" + suffix
		createNamespace(t, admin, name, map[string]string{"toolgate.soliddowant.dev/writable": "true"})
		grantAdmin(t, admin, name)

		_, err := gateway.CoreV1().Pods(name).Create(t.Context(), restrictedPod(name), metav1.CreateOptions{})
		refusedWith("may write only in namespaces labelled pod-security.kubernetes.io/enforce=restricted")(t, err)
	})

	// An instance namespace the gateway creates itself, and a shared namespace
	// labelled as Helm 4 installs the chart's one.
	instanceNamespace := "tg-e2e-podsecurity-" + suffix
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace, Labels: toolgateLabels}}
	_, err := gateway.CoreV1().Namespaces().Create(t.Context(), namespace, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		err := admin.CoreV1().Namespaces().Delete(context.Background(), instanceNamespace, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete namespace %s: %v", instanceNamespace, err)
		}
	})

	sharedNamespace := "e2e-shared-" + suffix
	createNamespace(t, admin, sharedNamespace, helmLabels)

	writable := []struct{ kind, name string }{
		{kind: "an instance namespace", name: instanceNamespace},
		{kind: "the shared namespace", name: sharedNamespace},
	}

	for _, target := range writable {
		namespace := target.name

		t.Run("a privileged host pod is refused in "+target.kind, func(t *testing.T) {
			_, err := gateway.CoreV1().Pods(namespace).Create(t.Context(), hostPod(namespace), metav1.CreateOptions{})
			refusedWith("violates PodSecurity")(t, err)
		})

		t.Run("a hostPath pod is refused in "+target.kind, func(t *testing.T) {
			_, err := gateway.CoreV1().Pods(namespace).Create(t.Context(), hostPathPod(namespace), metav1.CreateOptions{})
			refusedWith("violates PodSecurity")(t, err)
		})

		t.Run("a restricted pod is accepted in "+target.kind, func(t *testing.T) {
			pod := restrictedPod(namespace)
			_, err := gateway.CoreV1().Pods(namespace).Create(t.Context(), pod, metav1.CreateOptions{})
			require.NoError(t, err)

			err = gateway.CoreV1().Pods(namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
			require.NoError(t, err)
		})
	}

	t.Run("removing the Pod Security label from a namespace is refused", func(t *testing.T) {
		grantNamespaceUpdate(t, admin, "e2e-namespace-update-"+suffix)

		current, err := admin.CoreV1().Namespaces().Get(t.Context(), instanceNamespace, metav1.GetOptions{})
		require.NoError(t, err)

		delete(current.Labels, "pod-security.kubernetes.io/enforce")
		_, err = gateway.CoreV1().Namespaces().Update(t.Context(), current, metav1.UpdateOptions{})
		refusedWith("may not update namespaces")(t, err)
	})
}
