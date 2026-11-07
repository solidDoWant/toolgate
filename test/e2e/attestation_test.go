//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const (
	attestGroup      = "attest-devs"
	attestSecretName = "attest-token"
)

// workspacePodToken creates a pod in namespace labelled like a Coder
// workspace of owner and returns a token of the default service account
// bound to it, with the gateway's audience, as a projected volume would.
func workspacePodToken(t *testing.T, namespace, image, owner, workspace string) string {
	t.Helper()

	admin := adminClient(t)
	ctx := t.Context()

	pod, err := admin.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "workspace-",
			Namespace:    namespace,
			Labels: map[string]string{
				"com.coder.user.username":  owner,
				"com.coder.workspace.name": workspace,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           "default",
			AutomountServiceAccountToken: ptr.To(false),
			Containers:                   []corev1.Container{{Name: "workspace", Image: image, ImagePullPolicy: corev1.PullNever}},
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

// attestedHeader returns the headers of a request by user with the groups,
// the attestation token, and a claimed workspace.
func attestedHeader(t *testing.T, user string, groups []string, attestation, workspace string) http.Header {
	t.Helper()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+oidcProvider(t).MintAccessToken(t, testutil.TokenClaims{
		Subject: user, Email: user + "@example.com", EmailVerified: true, Groups: groups,
	}))
	header.Set(auth.SessionHeader, "e2e-"+rand.String(8))
	header.Set(auth.AttestationHeader, attestation)

	if workspace != "" {
		header.Set(auth.WorkspaceHeader, workspace)
	}

	return header
}

// attestationTemplate creates a template with access and returns its name.
func attestationTemplate(t *testing.T, kube client.Client, prefix, scope string, access v1alpha1.Access, resources []k8sruntime.RawExtension) string {
	t.Helper()

	name := prefix + "-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        scope,
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       access,
			Endpoint:     v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources:    resources,
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })
	requireTemplateValid(t, kube, name)

	return name
}

// secretServerResources is the test server with a Secret holding the user's
// secret attestSecretName, passed to the server as ATTEST_SECRET.
func secretServerResources(t *testing.T, image string) []k8sruntime.RawExtension {
	t.Helper()

	resources := testServerResources(t, image, 0, nil)

	var deployment map[string]any
	require.NoError(t, json.Unmarshal(resources[0].Raw, &deployment))

	containers := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	container := containers[0].(map[string]any)
	container["env"] = append(container["env"].([]any), map[string]any{
		"name":      "ATTEST_SECRET",
		"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "attest", "key": "value"}},
	})

	raw, err := json.Marshal(deployment)
	require.NoError(t, err)

	resources[0].Raw = raw

	secret, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": "attest"},
		"stringData": map[string]any{"value": "${secret:" + attestSecretName + "}"},
	})
	require.NoError(t, err)

	return append(resources, k8sruntime.RawExtension{Raw: secret})
}

// setUserSecret stores a secret for the user whose headers are given.
func setUserSecret(t *testing.T, endpoint string, header http.Header, name, value string) {
	t.Helper()

	base := strings.TrimSuffix(endpoint, gateway.MCPPath)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, base+"/api/v1/secrets/"+name, bytes.NewReader([]byte(value)))
	require.NoError(t, err)
	request.Header.Set("Authorization", header.Get("Authorization"))

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusOK, response.StatusCode)
}

// connectWithHeader opens an MCP session with the gateway and sends header
// on every request.
func connectWithHeader(t *testing.T, endpoint string, header http.Header) *gatewaySession {
	t.Helper()

	observed := &gatewaySession{}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { observed.record(eventListChanged) },
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) {
			observed.record(eventProgress)
		},
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

func TestAttestedUserSessions(t *testing.T) {
	namespace := workloadNamespace(t)
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)
	allowed := testutil.NewSPIFFETrustDomain(t, "allowed.test")
	spiffeBundleEndpoint(t, allowed)

	secretServer := attestationTemplate(t, kube, "e2e-attest-secret", "per-user",
		v1alpha1.Access{Users: []string{"alice"}, Environments: "any"}, secretServerResources(t, image))
	optInServer := attestationTemplate(t, kube, "e2e-attest-optin", "per-session",
		v1alpha1.Access{Users: []string{"alice"}, Environments: "opt-in"}, testServerResources(t, image, 0, nil))
	groupServer := attestationTemplate(t, kube, "e2e-attest-group", "per-session",
		v1alpha1.Access{Groups: []string{attestGroup}, Environments: "any"}, testServerResources(t, image, 0, nil))

	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	groups := []string{attestGroup, "other-group"}

	connect := func(t *testing.T, header http.Header) *gatewaySession {
		t.Helper()

		return connectWithHeader(t, endpoint, header)
	}

	identityOf := func(t *testing.T, header http.Header) string {
		t.Helper()

		var identity string

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			identity = gatewayQuery(t, admin, "SELECT DISTINCT attestation || ' ' || workspace FROM audit_events "+
				"WHERE session = '"+header.Get(auth.SessionHeader)+"' AND type = 'session.start'")
			assert.NotEmpty(collect, identity)
		}, time.Minute, time.Second)

		return strings.TrimSpace(identity)
	}

	t.Run("a user token plus a pod's projected token is attested in the coder workspace", func(t *testing.T) {
		token := workspacePodToken(t, namespace, image, "alice", "dev")
		header := attestedHeader(t, "alice", groups, token, "claimed-ws-"+rand.String(4))
		session := connect(t, header)

		assert.Equal(t, "attested coder/alice/dev", identityOf(t, header), "the verified workspace wins over the claimed one")

		setUserSecret(t, endpoint, header, attestSecretName, "alice-attested-secret")

		t.Cleanup(func() {
			request, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodDelete, strings.TrimSuffix(endpoint, gateway.MCPPath)+"/api/v1/secrets/"+attestSecretName, nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", header.Get("Authorization"))

			if response, err := http.DefaultClient.Do(request); err == nil {
				_ = response.Body.Close()
			}
		})

		assert.ElementsMatch(t, prefixedTools(secretServer), enable(t, session, secretServer), "the user's per-user instance")

		var environment map[string]string
		structuredResult(t, session.session, secretServer+"__env", &environment)
		assert.Equal(t, "alice-attested-secret", environment["ATTEST_SECRET"], "the user's own secret")

		entry, found := serverEntry(t, t.Context(), session.session, optInServer)
		require.True(t, found)
		assert.NotEqual(t, gateway.StatusBlocked, entry.Status, "an opt-in server passes without a grant: %s", entry.Reason)

		entry, found = serverEntry(t, t.Context(), session.session, groupServer)
		require.True(t, found, "the user's group gives access")
		assert.NotEqual(t, gateway.StatusBlocked, entry.Status, entry.Reason)
	})

	t.Run("a pod labelled for another user falls back to the user with the claimed workspace", func(t *testing.T) {
		claimed := "claimed-ws-" + rand.String(4)
		token := workspacePodToken(t, namespace, image, "bob", "dev")
		header := attestedHeader(t, "alice", groups, token, claimed)
		session := connect(t, header)

		assert.Equal(t, "user claimed/"+claimed, identityOf(t, header))

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			count := gatewayQuery(t, admin, "SELECT count(*) FROM audit_events WHERE type = 'auth.refuse' AND principal = 'alice' "+
				"AND details->>'reason' = 'attestation owner mismatch'")
			assert.NotEqual(collect, "0", strings.TrimSpace(count))
		}, time.Minute, time.Second)

		entry, found := serverEntry(t, t.Context(), session.session, optInServer)
		require.True(t, found)
		assert.Equal(t, gateway.StatusBlocked, entry.Status, "an opt-in server needs a grant without an attestation")
	})

	t.Run("a JWT-SVID bound by the SPIFFE ID pattern attests the user", func(t *testing.T) {
		svid := allowed.MintSVID(t, "/coder/alice/dev")

		// The gateway fetches the bundle soon after the endpoint starts, so
		// early attempts fall back to the user.
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			header := attestedHeader(t, "alice", groups, svid, "")
			connect(t, header)
			assert.Equal(collect, "attested coder/alice/dev", identityOf(t, header))
		}, 2*time.Minute, 2*time.Second)
	})
}
