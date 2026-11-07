//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

const (
	remoteSecretName = "remote-api-key"
	remoteAliceKey   = "alice-remote-key"
	remoteBobKey     = "bob-remote-key"
)

// deployBearerServer runs the test MCP server in bearer-token mode in a new
// namespace behind a Service, and returns the Service's cluster URL.
func deployBearerServer(t *testing.T, admin *kubernetes.Clientset, image string) string {
	t.Helper()

	namespace := "e2e-remote-" + rand.String(6)
	createNamespace(t, admin, namespace, nil)

	labels := map[string]string{"app": "bearer-mcp"}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "bearer-mcp"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)},
					Containers: []corev1.Container{{
						Name:            "mcp",
						Image:           image,
						ImagePullPolicy: corev1.PullNever,
						Env: []corev1.EnvVar{
							{Name: "MCPSERVER_TOOLS", Value: "whoami,echo"},
							{Name: "MCPSERVER_BEARER_TOKENS", Value: remoteAliceKey + "=alice," + remoteBobKey + "=bob"},
						},
						Ports: []corev1.ContainerPort{{Name: "mcp", ContainerPort: 8080}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}},
							PeriodSeconds: 1,
						},
					}},
				},
			},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "bearer-mcp"},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: "mcp", Port: 8080, TargetPort: intstr.FromInt32(8080)}},
		},
	}

	_, err := admin.AppsV1().Deployments(namespace).Create(t.Context(), deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = admin.CoreV1().Services(namespace).Create(t.Context(), service, metav1.CreateOptions{})
	require.NoError(t, err)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		current, err := admin.AppsV1().Deployments(namespace).Get(t.Context(), deployment.Name, metav1.GetOptions{})
		require.NoError(collect, err)
		assert.Equal(collect, int32(1), current.Status.ReadyReplicas)
	}, 2*time.Minute, time.Second)

	return "http://bearer-mcp." + namespace + ".svc.cluster.local:8080/mcp"
}

func createRemoteTemplate(t *testing.T, url string) string {
	t.Helper()

	kube := templateClient(t)
	name := "e2e-remote-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:  "per-user",
			Access: v1alpha1.Access{Users: []string{"alice", "bob", "carol", "dave"}, Environments: "any"},
			Remote: &v1alpha1.RemoteServer{
				URL:     url,
				Headers: map[string]string{"Authorization": "Bearer ${secret:" + remoteSecretName + "}"},
			},
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })
	requireTemplateValid(t, kube, name)

	return name
}

// A remote template connects to a server outside the gateway's control with
// headers built from the calling user's own secret.
func TestRemoteServerWithBearerToken(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)

	server := createRemoteTemplate(t, deployBearerServer(t, admin, image))
	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])

	connect := func(t *testing.T, user string) (*gatewaySession, http.Header) {
		t.Helper()

		header := gatewayHeader(t, user, "remote-"+user+"-"+rand.String(5), nil)

		return connectWithHeader(t, endpoint, header), header
	}

	setSecret := func(t *testing.T, header http.Header, value string) {
		t.Helper()

		setUserSecret(t, endpoint, header, remoteSecretName, value)
	}

	whoami := func(t *testing.T, session *gatewaySession) string {
		t.Helper()

		var result map[string]string
		structuredResult(t, session.session, server+"__whoami", &result)

		return result["user"]
	}

	t.Run("with the secret set a user lists and calls a tool", func(t *testing.T) {
		session, header := connect(t, "alice")
		setSecret(t, header, remoteAliceKey)

		assert.ElementsMatch(t, []string{server + "__whoami", server + "__echo"}, enable(t, session, server))
		assert.Equal(t, "alice", whoami(t, session))
	})

	t.Run("with the secret missing the user is told to set it", func(t *testing.T) {
		session, _ := connect(t, "carol")

		result := awaitEnable(t, startEnable(t, session, server))
		assert.Contains(t, result.err, "toolgate secret set "+remoteSecretName)
	})

	t.Run("with a wrong key the user is told the credential was refused", func(t *testing.T) {
		session, header := connect(t, "dave")
		setSecret(t, header, "not-the-key")

		result := awaitEnable(t, startEnable(t, session, server))
		assert.Contains(t, result.err, "refused your credential")
		assert.Contains(t, result.err, "toolgate secret set "+remoteSecretName)
		assert.NotContains(t, result.err, "not-the-key")
	})

	t.Run("a second user's call carries the second user's key", func(t *testing.T) {
		aliceSession, aliceHeader := connect(t, "alice")
		setSecret(t, aliceHeader, remoteAliceKey)
		bobSession, bobHeader := connect(t, "bob")
		setSecret(t, bobHeader, remoteBobKey)

		enable(t, aliceSession, server)
		enable(t, bobSession, server)

		assert.Equal(t, "bob", whoami(t, bobSession))
		assert.Equal(t, "alice", whoami(t, aliceSession))
	})
}
