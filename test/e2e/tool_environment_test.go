//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
)

// workloadNamespace is the namespace whose pods' service account tokens the
// gateway accepts in the e2e install.
func workloadNamespace(t *testing.T) string {
	t.Helper()

	namespace := os.Getenv("TOOLGATE_E2E_WORKLOAD_NAMESPACE")
	if namespace == "" {
		t.Skip("TOOLGATE_E2E_WORKLOAD_NAMESPACE is not set (run through scripts/e2e/run.sh)")
	}

	return namespace
}

// podToken creates a pod in namespace and returns a token of its service
// account bound to it, with the gateway's audience, as an attested workload
// would mount it.
func podToken(t *testing.T, namespace, image string) string {
	t.Helper()

	admin := adminClient(t)
	ctx := t.Context()

	pod, err := admin.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "attested-", Namespace: namespace},
		Spec: corev1.PodSpec{
			ServiceAccountName:           "default",
			AutomountServiceAccountToken: ptr.To(false),
			Containers:                   []corev1.Container{{Name: "workload", Image: image, ImagePullPolicy: corev1.PullNever}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = admin.CoreV1().Pods(namespace).Delete(context.WithoutCancel(ctx), pod.Name, metav1.DeleteOptions{})
	})

	token, err := admin.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, "default", &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{auth.ServiceAccountAudience},
			ExpirationSeconds: ptr.To[int64](3600),
			BoundObjectRef:    &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	return token.Status.Token
}

// connectWithToken opens an MCP session with the gateway with a raw bearer
// token, such as a pod's service account token.
func connectWithToken(t *testing.T, endpoint, token, session string) *gatewaySession {
	t.Helper()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set(auth.SessionHeader, session)

	observed := &gatewaySession{}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { observed.record(eventListChanged) },
	})

	connected, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearerTransport{header: header}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = connected.Close() })

	observed.session = connected

	return observed
}

func TestAC2_10PerToolEnvironment(t *testing.T) {
	image := mcpServerImage(t)
	namespace := workloadNamespace(t)
	admin := adminClient(t)
	kube := templateClient(t)

	const attestedTool = "env"

	name := "e2e-tool-env-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "per-user",
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access: v1alpha1.Access{
				Users:           []string{"alice"},
				ServiceAccounts: []string{"system:serviceaccount:" + namespace + ":default"},
				Environments:    "any",
			},
			Tools:     map[string]v1alpha1.ToolOverride{attestedTool: {Environments: "attested"}},
			Endpoint:  v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources: testServerResources(t, image, 0, nil),
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })
	requireTemplateValid(t, kube, name)

	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])

	t.Run("[AC-2-10] a tool marked environments: attested is missing from the laptop user's tool list", func(t *testing.T) {
		laptop := connectGateway(t, endpoint, "alice", "e2e-"+rand.String(8))
		enable(t, laptop, name)

		listed := listedTools(t, t.Context(), laptop.session)
		assert.Contains(t, listed, name+"__echo")
		assert.NotContains(t, listed, name+"__"+attestedTool)
	})

	t.Run("[AC-2-10] the same tool is present for an attested pod", func(t *testing.T) {
		pod := connectWithToken(t, endpoint, podToken(t, namespace, image), "e2e-"+rand.String(8))
		enable(t, pod, name)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			listed := listedTools(collect, t.Context(), pod.session)
			assert.Contains(collect, listed, name+"__echo")
			assert.Contains(collect, listed, name+"__"+attestedTool)
		}, time.Minute, 100*time.Millisecond)
	})
}
