//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// e2eSharedNamespace is the shared namespace scripts/e2e/run.sh configures.
const e2eSharedNamespace = "toolgate-shared"

// sharedNamespaceResources are the test MCP server's Deployment and Service,
// named and selected by ${instance.id} as a `namespace: shared` template
// requires, and a Secret.
func sharedNamespaceResources(t *testing.T, image string) []k8sruntime.RawExtension {
	t.Helper()

	return sharedNamespaceResourcesWithTools(t, image, testServerTools)
}

// sharedNamespaceResourcesWithTools is sharedNamespaceResources for a test
// MCP server that serves tools.
func sharedNamespaceResourcesWithTools(t *testing.T, image string, tools []string) []k8sruntime.RawExtension {
	t.Helper()

	instanceLabels := map[string]any{"app": "mcp", runtime.LabelInstance: "${instance.id}"}

	var resources []k8sruntime.RawExtension

	for _, resource := range testServerResourcesWithTools(t, image, tools, 0, nil) {
		var object map[string]any
		require.NoError(t, json.Unmarshal(resource.Raw, &object))

		object["metadata"] = map[string]any{"name": "mcp-${instance.id}"}

		spec := object["spec"].(map[string]any)
		switch object["kind"] {
		case "Deployment":
			spec["selector"] = map[string]any{"matchLabels": instanceLabels}
			spec["template"].(map[string]any)["metadata"] = map[string]any{"labels": instanceLabels}
		case "Service":
			spec["selector"] = instanceLabels
		}

		raw, err := json.Marshal(object)
		require.NoError(t, err)

		resources = append(resources, k8sruntime.RawExtension{Raw: raw})
	}

	secret, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "creds-${instance.id}"},
		"stringData": map[string]any{"token": "not-a-secret"},
	})
	require.NoError(t, err)

	return append(resources, k8sruntime.RawExtension{Raw: secret})
}

// requireSecretGone waits until the Secret is deleted from the shared
// namespace.
func requireSecretGone(t *testing.T, admin *kubernetes.Clientset, name, why string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		_, err := admin.CoreV1().Secrets(e2eSharedNamespace).Get(t.Context(), name, metav1.GetOptions{})
		assert.True(collect, apierrors.IsNotFound(err), "%s: %v", why, err)
	}, 3*time.Minute, 2*time.Second)
}

// Objects of an instance in the shared namespace, Secrets included, are
// owned by the instance's anchor ConfigMap, so the garbage collector deletes
// them when the anchor is deleted. The gateway never reads Secrets.
func TestSharedNamespaceSecretsAreDeleted(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	t.Run("stopping a shared-namespace instance deletes its Secret", func(t *testing.T) {
		name := "e2e-shared-ns-" + rand.String(5)
		template := &v1alpha1.MCPServerTemplate{
			ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
			Spec: v1alpha1.MCPServerTemplateSpec{
				Scope:        "shared",
				Namespace:    "shared",
				StartTimeout: &metav1.Duration{Duration: enableTimeout},
				Access:       v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"},
				Endpoint:     v1alpha1.Endpoint{Service: "mcp-${instance.id}", Port: 8080, Path: "/mcp"},
				Resources:    sharedNamespaceResources(t, image),
			},
		}
		require.NoError(t, kube.Create(t.Context(), template))
		t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })
		requireTemplateValid(t, kube, name)

		pods := gatewayPods(t, admin)
		alice := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))
		assert.ElementsMatch(t, prefixedTools(name), enable(t, alice, name))

		anchors, err := admin.CoreV1().ConfigMaps(e2eSharedNamespace).List(t.Context(), metav1.ListOptions{
			LabelSelector: runtime.LabelTemplate + "=" + name,
		})
		require.NoError(t, err)

		var anchor *corev1.ConfigMap

		for index := range anchors.Items {
			if anchors.Items[index].Name == runtime.AnchorName(anchors.Items[index].Labels[runtime.LabelInstance]) {
				anchor = &anchors.Items[index]
			}
		}

		require.NotNil(t, anchor, "the instance has an anchor ConfigMap")

		secretName := "creds-" + anchor.Labels[runtime.LabelInstance]
		secret, err := admin.CoreV1().Secrets(e2eSharedNamespace).Get(t.Context(), secretName, metav1.GetOptions{})
		require.NoError(t, err)
		require.Len(t, secret.OwnerReferences, 1)
		assert.Equal(t, anchor.UID, secret.OwnerReferences[0].UID, "the anchor owns the Secret")

		// Deleting the template stops its instances.
		require.NoError(t, kube.Delete(t.Context(), template))

		requireSecretGone(t, admin, secretName, "the stopped instance's Secret is deleted")
	})

	t.Run("the sweep deletes the Secret of an instance that isn't live", func(t *testing.T) {
		id := rand.String(32)
		labels := map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue, runtime.LabelInstance: id}

		anchor, err := admin.CoreV1().ConfigMaps(e2eSharedNamespace).Create(t.Context(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: runtime.AnchorName(id), Labels: labels},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		secret, err := admin.CoreV1().Secrets(e2eSharedNamespace).Create(t.Context(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "creds-" + id, Labels: labels,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: anchor.Name, UID: anchor.UID}},
			},
			StringData: map[string]string{"token": "not-a-secret"},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		requireSecretGone(t, admin, secret.Name, "the orphaned instance's Secret is deleted")
	})
}
