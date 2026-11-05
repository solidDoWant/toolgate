//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/rest"
)

// mcpServerImage returns the test MCP server image the harness loaded into
// the cluster, or skips the test.
func mcpServerImage(t *testing.T) string {
	t.Helper()

	image := os.Getenv("TOOLGATE_E2E_MCPSERVER_IMAGE")
	if image == "" {
		t.Skip("TOOLGATE_E2E_MCPSERVER_IMAGE is not set (run through scripts/e2e/run.sh)")
	}

	return image
}

// connectThroughProxy connects an MCP client to port 8080 of a pod through
// the API server's pod proxy.
func connectThroughProxy(t *testing.T, config *rest.Config, namespace, pod string) *mcp.ClientSession {
	t.Helper()

	httpClient, err := rest.HTTPClientFor(config)
	require.NoError(t, err)

	transport := &mcp.StreamableClientTransport{
		Endpoint:   config.Host + "/api/v1/namespaces/" + namespace + "/pods/" + pod + ":8080/proxy/mcp",
		HTTPClient: httpClient,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), transport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	return session
}

func structuredResult(t *testing.T, session *mcp.ClientSession, tool string, result any) {
	t.Helper()

	response, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool})
	require.NoError(t, err)
	require.False(t, response.IsError, "tool %s failed: %v", tool, response.Content)

	encoded, err := json.Marshal(response.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, result))
}

// The harness's test MCP server image runs in the cluster and reports the
// environment and mounted Secret it was given, with the configured tools.
func TestHarnessMCPServerImage(t *testing.T) {
	image := mcpServerImage(t)
	config := adminConfig(t)
	admin := adminClient(t)

	namespace := "e2e-mcpserver-" + rand.String(6)
	createNamespace(t, admin, namespace, nil)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "credentials"},
		StringData: map[string]string{"token": "e2e-token"},
	}
	_, err := admin.CoreV1().Secrets(namespace).Create(t.Context(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	nonRoot := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mcpserver"},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &nonRoot},
			Containers: []corev1.Container{{
				Name:            "mcpserver",
				Image:           image,
				ImagePullPolicy: corev1.PullNever,
				Env: []corev1.EnvVar{
					{Name: "GREETING", Value: "hello from e2e"},
					{Name: "MCPSERVER_TOOLS", Value: "env,secrets,read_file"},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "credentials", MountPath: "/secrets", ReadOnly: true}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}},
					PeriodSeconds: 1,
				},
			}},
			Volumes: []corev1.Volume{{
				Name:         "credentials",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "credentials"}},
			}},
		},
	}
	_, err = admin.CoreV1().Pods(namespace).Create(t.Context(), pod, metav1.CreateOptions{})
	require.NoError(t, err)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		current, err := admin.CoreV1().Pods(namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
		require.NoError(collect, err)
		assert.True(collect, podReady(current), "pod %s is not ready: %+v", pod.Name, current.Status)
	}, 2*time.Minute, time.Second)

	session := connectThroughProxy(t, config, namespace, pod.Name)

	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}

	assert.ElementsMatch(t, []string{"env", "secrets", "read_file"}, names)

	var environment map[string]string
	structuredResult(t, session, "env", &environment)
	assert.Equal(t, "hello from e2e", environment["GREETING"])

	var files map[string]string
	structuredResult(t, session, "secrets", &files)
	assert.Equal(t, map[string]string{"token": "e2e-token"}, files)
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
