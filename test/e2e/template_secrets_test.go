//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// templateSecretsNamespace is the namespace scripts/e2e/run.sh configures as
// config.templateSecretsNamespace.
func templateSecretsNamespace(t *testing.T) string {
	t.Helper()

	namespace := os.Getenv("TOOLGATE_E2E_TEMPLATE_SECRETS_NAMESPACE")
	if namespace == "" {
		t.Skip("TOOLGATE_E2E_TEMPLATE_SECRETS_NAMESPACE is not set (run through scripts/e2e/run.sh)")
	}

	return namespace
}

// templateSecretResources is the shared-namespace test server with a Secret
// whose only key is ${templateSecret:source/key}, mounted at /secrets.
func templateSecretResources(t *testing.T, image, source string) []k8sruntime.RawExtension {
	t.Helper()

	resources := sharedNamespaceResourcesWithTools(t, image, []string{"echo", "secrets"})

	var deployment map[string]any
	require.NoError(t, json.Unmarshal(resources[0].Raw, &deployment))

	podSpec := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	podSpec["volumes"] = []any{map[string]any{
		"name":   "credentials",
		"secret": map[string]any{"secretName": "creds-${instance.id}"},
	}}
	container := podSpec["containers"].([]any)[0].(map[string]any)
	container["volumeMounts"] = []any{map[string]any{"name": "credentials", "mountPath": "/secrets", "readOnly": true}}

	raw, err := json.Marshal(deployment)
	require.NoError(t, err)

	resources[0].Raw = raw

	secret, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "creds-${instance.id}"},
		"stringData": map[string]any{"token": "${templateSecret:" + source + "/key}"},
	})
	require.NoError(t, err)

	resources[len(resources)-1] = k8sruntime.RawExtension{Raw: secret}

	return resources
}

// A shared-namespace instance gets the template secret's value when it
// starts, and the gateway patches a changed value into the instance's Secret
// without a restart.
func TestTemplateSecretSharedInstance(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)
	namespace := templateSecretsNamespace(t)

	source := "e2e-source-" + rand.String(5)
	_, err := admin.CoreV1().Secrets(namespace).Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: source},
		StringData: map[string]string{"key": "first-value"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = admin.CoreV1().Secrets(namespace).Delete(context.WithoutCancel(t.Context()), source, metav1.DeleteOptions{})
	})

	name := "e2e-template-secret-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "shared",
			Namespace:    "shared",
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       v1alpha1.Access{Users: []string{"alice"}, Environments: "any"},
			Endpoint:     v1alpha1.Endpoint{Service: "mcp-${instance.id}", Port: 8080, Path: "/mcp"},
			Resources:    templateSecretResources(t, image, source),
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })
	requireTemplateValid(t, kube, name)

	pods := gatewayPods(t, admin)
	alice := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))
	require.NotEmpty(t, enable(t, alice, name))

	var files map[string]string

	structuredResult(t, alice.session, name+"__secrets", &files)
	assert.Equal(t, map[string]string{"token": "first-value"}, files, "the pod sees the value from its first start")

	anchors, err := admin.CoreV1().ConfigMaps(e2eSharedNamespace).List(t.Context(), metav1.ListOptions{
		LabelSelector: runtime.LabelTemplate + "=" + name,
	})
	require.NoError(t, err)
	require.NotEmpty(t, anchors.Items)

	instanceID := anchors.Items[0].Labels[runtime.LabelInstance]
	target := "creds-" + instanceID

	podsBefore, err := admin.CoreV1().Pods(e2eSharedNamespace).List(t.Context(), metav1.ListOptions{
		LabelSelector: runtime.LabelInstance + "=" + instanceID,
	})
	require.NoError(t, err)
	require.Len(t, podsBefore.Items, 1)

	current, err := admin.CoreV1().Secrets(namespace).Get(t.Context(), source, metav1.GetOptions{})
	require.NoError(t, err)

	current.Data = map[string][]byte{"key": []byte("second-value")}
	_, err = admin.CoreV1().Secrets(namespace).Update(t.Context(), current, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		copied, err := admin.CoreV1().Secrets(e2eSharedNamespace).Get(t.Context(), target, metav1.GetOptions{})
		require.NoError(collect, err)
		assert.Equal(collect, "second-value", string(copied.Data["token"]), "the shared-namespace Secret is patched")
	}, enableTimeout, 2*time.Second)

	podsAfter, err := admin.CoreV1().Pods(e2eSharedNamespace).List(t.Context(), metav1.ListOptions{
		LabelSelector: runtime.LabelInstance + "=" + instanceID,
	})
	require.NoError(t, err)
	require.Len(t, podsAfter.Items, 1)
	assert.Equal(t, podsBefore.Items[0].UID, podsAfter.Items[0].UID, "the update restarts nothing")
}

// Kubernetes RBAC, not only the gateway, limits the gateway's Secret reads to
// the template secrets namespace.
func TestTemplateSecretsRBACConfinedToNamespace(t *testing.T) {
	admin := adminClient(t)
	namespace := templateSecretsNamespace(t)

	// An instance namespace exists only while an instance runs, so make one
	// with the labels the gateway gives its own.
	instanceNamespace := "tg-rbac-" + rand.String(5)
	_, err := admin.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   instanceNamespace,
			Labels: map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = admin.CoreV1().Namespaces().Delete(context.WithoutCancel(t.Context()), instanceNamespace, metav1.DeleteOptions{})
	})

	user := "system:serviceaccount:" + releaseNamespace() + ":" + gatewayServiceAccount

	tests := []struct {
		name      string
		namespace string
		allowed   bool
	}{
		{"template secrets namespace", namespace, true},
		// The release namespace is also the catalog namespace here.
		{"gateway and catalog namespace", releaseNamespace(), false},
		{"shared namespace", e2eSharedNamespace, false},
		{"instance namespace", instanceNamespace, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, verb := range []string{"get", "list", "watch"} {
				assert.Equal(t, test.allowed, canGatewayDo(t, admin, user, test.namespace, verb), "%s secrets in %s", verb, test.namespace)
			}
		})
	}
}

// canGatewayDo asks the API server whether the gateway's service account may
// do verb on Secrets in namespace.
func canGatewayDo(t *testing.T, admin *kubernetes.Clientset, user, namespace, verb string) bool {
	t.Helper()

	review, err := admin.AuthorizationV1().SubjectAccessReviews().Create(t.Context(), &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User: user,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Verb: verb, Resource: "secrets",
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	return review.Status.Allowed
}
