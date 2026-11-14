//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gatewayDeployment is the name of the Deployment the chart creates for a
// release named toolgate.
const gatewayDeployment = "toolgate"

func TestChartInstall(t *testing.T) {
	admin := adminClient(t)

	t.Run("the CRDs exist", func(t *testing.T) {
		extensions, err := apiextensionsclient.NewForConfig(adminConfig(t))
		require.NoError(t, err)

		for _, name := range []string{
			"mcpservertemplates.toolgate.soliddowant.dev",
			"toolgaterules.toolgate.soliddowant.dev",
			"settingsprofiles.toolgate.soliddowant.dev",
			"tokenexchanges.toolgate.soliddowant.dev",
		} {
			_, err := extensions.ApiextensionsV1().CustomResourceDefinitions().Get(t.Context(), name, metav1.GetOptions{})
			assert.NoError(t, err, name)
		}
	})

	t.Run("two replicas are ready and /healthz answers on both", func(t *testing.T) {
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			deployment, err := admin.AppsV1().Deployments(releaseNamespace()).Get(t.Context(), gatewayDeployment, metav1.GetOptions{})
			require.NoError(collect, err)
			assert.Equal(collect, int32(2), deployment.Status.ReadyReplicas)
		}, 5*time.Minute, 2*time.Second)

		deployment, err := admin.AppsV1().Deployments(releaseNamespace()).Get(t.Context(), gatewayDeployment, metav1.GetOptions{})
		require.NoError(t, err)

		selector := metav1.FormatLabelSelector(deployment.Spec.Selector)
		pods, err := admin.CoreV1().Pods(releaseNamespace()).List(t.Context(), metav1.ListOptions{LabelSelector: selector})
		require.NoError(t, err)

		var ready []corev1.Pod

		for _, pod := range pods.Items {
			if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
				ready = append(ready, pod)
			}
		}

		require.Len(t, ready, 2)

		for _, pod := range ready {
			body, err := admin.CoreV1().Pods(releaseNamespace()).ProxyGet("http", pod.Name, "8080", "/healthz", nil).DoRaw(t.Context())
			require.NoError(t, err, "pod %s", pod.Name)
			assert.NotEmpty(t, body, "pod %s", pod.Name)
		}
	})
}
