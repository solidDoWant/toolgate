//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
)

// toolgateLabels are the labels the admission policy requires on every
// namespace the gateway writes to.
var toolgateLabels = map[string]string{
	"app.kubernetes.io/managed-by":       "toolgate",
	"pod-security.kubernetes.io/enforce": "restricted",
}

func instanceLabels(instance string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":      "toolgate",
		"toolgate.soliddowant.dev/instance": instance,
		"toolgate.soliddowant.dev/template": "e2e",
	}
}

// createNamespace creates a namespace as the cluster admin and deletes it when
// the test ends.
func createNamespace(t *testing.T, admin *kubernetes.Clientset, name string, labels map[string]string) {
	t.Helper()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	_, err := admin.CoreV1().Namespaces().Create(t.Context(), namespace, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		err := admin.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete namespace %s: %v", name, err)
		}
	})
}

// grantAdmin binds the gateway's service account to the built-in admin role
// in namespace, so a refusal there comes from the admission policy, not RBAC.
func grantAdmin(t *testing.T, admin *kubernetes.Clientset, namespace string) {
	t.Helper()

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-gateway-admin", Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "admin"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: gatewayServiceAccount, Namespace: releaseNamespace()}},
	}
	_, err := admin.RbacV1().RoleBindings(namespace).Create(t.Context(), binding, metav1.CreateOptions{})
	require.NoError(t, err)
}

func refusedWith(message string) require.ErrorAssertionFunc {
	return func(t require.TestingT, err error, _ ...any) {
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err) || apierrors.IsInvalid(err), "refused, got %v", err)
		assert.Contains(t, err.Error(), message)
	}
}

func TestAC1_24GatewayWritesAreConfined(t *testing.T) {
	admin := adminClient(t)
	gateway := gatewayClient(t)

	suffix := rand.String(8)
	instance := "e2e" + suffix
	instanceNamespace := "tg-" + instance
	plainNamespace := "e2e-plain-" + suffix

	// The namespace carries toolgate's managed-by and Pod Security labels,
	// which are all the admission policy looks at, but not the instance label: the orphan sweep
	// deletes every namespace with that label and no live instance row behind
	// it, and this namespace has none.
	createNamespace(t, admin, instanceNamespace, toolgateLabels)
	createNamespace(t, admin, plainNamespace, nil)
	grantAdmin(t, admin, instanceNamespace)
	grantAdmin(t, admin, plainNamespace)

	t.Run("[AC-1-24] a ClusterRole is refused", func(t *testing.T) {
		role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "e2e-" + suffix, Labels: instanceLabels(instance)}}
		_, err := gateway.RbacV1().ClusterRoles().Create(t.Context(), role, metav1.CreateOptions{})
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err), "refused, got %v", err)
	})

	t.Run("[AC-1-24] a Role with the instance label in an instance namespace is refused", func(t *testing.T) {
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: instanceNamespace, Labels: instanceLabels(instance)}}
		_, err := gateway.RbacV1().Roles(instanceNamespace).Create(t.Context(), role, metav1.CreateOptions{})
		refusedWith("may not write Role objects")(t, err)
	})

	configMap := func(namespace string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "e2e", Namespace: namespace, Labels: instanceLabels(instance)}}
	}

	t.Run("an allowed kind in an instance namespace is accepted", func(t *testing.T) {
		_, err := gateway.CoreV1().ConfigMaps(instanceNamespace).Create(t.Context(), configMap(instanceNamespace), metav1.CreateOptions{})
		require.NoError(t, err)

		err = gateway.CoreV1().ConfigMaps(instanceNamespace).Delete(t.Context(), "e2e", metav1.DeleteOptions{})
		require.NoError(t, err)
	})

	t.Run("an allowed kind outside toolgate's namespaces is refused", func(t *testing.T) {
		_, err := gateway.CoreV1().ConfigMaps(plainNamespace).Create(t.Context(), configMap(plainNamespace), metav1.CreateOptions{})
		refusedWith("may write only in namespaces labelled")(t, err)
	})

	namespaceTests := []struct {
		name         string
		namespace    string
		labels       map[string]string
		errAssertion require.ErrorAssertionFunc
	}{
		{name: "an instance namespace is accepted", namespace: "tg-e2e-new-" + suffix, labels: toolgateLabels},
		{
			name: "a namespace without the tg- prefix is refused", namespace: "e2e-new-" + suffix, labels: toolgateLabels,
			errAssertion: refusedWith("may create only namespaces named tg-*"),
		},
		{
			name: "a namespace without toolgate's label is refused", namespace: "tg-e2e-unlabelled-" + suffix,
			labels:       map[string]string{"pod-security.kubernetes.io/enforce": "restricted"},
			errAssertion: refusedWith("may create only namespaces named tg-*"),
		},
		{
			name: "a namespace without Pod Security restricted is refused", namespace: "tg-e2e-unrestricted-" + suffix,
			labels:       map[string]string{"app.kubernetes.io/managed-by": "toolgate"},
			errAssertion: refusedWith("may create only namespaces named tg-*"),
		},
		{
			name: "a namespace with a weaker Pod Security level is refused", namespace: "tg-e2e-privileged-" + suffix,
			labels:       map[string]string{"app.kubernetes.io/managed-by": "toolgate", "pod-security.kubernetes.io/enforce": "privileged"},
			errAssertion: refusedWith("may create only namespaces named tg-*"),
		},
	}

	for _, test := range namespaceTests {
		t.Run(test.name, func(t *testing.T) {
			if test.errAssertion == nil {
				test.errAssertion = require.NoError
			}

			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: test.namespace, Labels: test.labels}}
			_, err := gateway.CoreV1().Namespaces().Create(t.Context(), namespace, metav1.CreateOptions{})
			test.errAssertion(t, err)

			if err == nil {
				err = gateway.CoreV1().Namespaces().Delete(t.Context(), test.namespace, metav1.DeleteOptions{})
				require.NoError(t, err)
			}
		})
	}

	t.Run("deleting a namespace without toolgate's label is refused", func(t *testing.T) {
		err := gateway.CoreV1().Namespaces().Delete(t.Context(), plainNamespace, metav1.DeleteOptions{})
		refusedWith("may delete only namespaces labelled")(t, err)
	})
}
