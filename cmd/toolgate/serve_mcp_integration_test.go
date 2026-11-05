//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const (
	serveLeaseNamespace   = "toolgate-system"
	serveCatalogNamespace = "catalog"
)

// writeKubeconfig writes a kubeconfig for the envtest API server and points
// KUBECONFIG at it.
func writeKubeconfig(t *testing.T, kube *testutil.Envtest) {
	t.Helper()

	config := clientcmdapi.NewConfig()
	config.Clusters["envtest"] = &clientcmdapi.Cluster{Server: kube.Config.Host, CertificateAuthorityData: kube.Config.CAData}
	config.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: kube.Config.CertData, ClientKeyData: kube.Config.KeyData, Token: kube.Config.BearerToken,
	}
	config.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	config.CurrentContext = "envtest"

	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(*config, path))
	t.Setenv("KUBECONFIG", path)
}

// headerTransport adds fixed headers to every request.
type headerTransport struct{ header http.Header }

func (h headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for name, values := range h.header {
		request.Header[name] = values
	}

	return http.DefaultTransport.RoundTrip(request)
}

// gatewayAddress returns the main port address that serve logged.
func gatewayAddress(t *testing.T, stdout *lockedBuffer) string {
	t.Helper()

	for line := range strings.SplitSeq(stdout.String(), "\n") {
		var entry struct{ Msg, Address string }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "gateway started" {
			return entry.Address
		}
	}

	require.FailNow(t, "serve logged no address")

	return ""
}

func listServerNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: gateway.ToolListServers})
	require.NoError(t, err)
	require.False(t, result.IsError, "list_servers failed: %v", result.Content)

	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)

	var decoded gateway.ListServersResult
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	var names []string
	for _, server := range decoded.Servers {
		names = append(names, server.Name)
	}

	return names
}

func TestServeServesTheCatalogAndStartsInstances(t *testing.T) {
	clearConfigEnv(t)

	pg := testutil.StartPostgres(t)
	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	kube := testutil.StartEnvtest(t)
	writeKubeconfig(t, kube)

	for _, name := range []string{serveLeaseNamespace, serveCatalogNamespace} {
		require.NoError(t, kube.Client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}

	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: serveCatalogNamespace, Name: "notes"},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:     catalog.ScopePerUser,
			Namespace: catalog.NamespacePerInstance,
			Endpoint:  v1alpha1.Endpoint{Service: "mcp", Port: 8080},
			Access:    v1alpha1.Access{Groups: []string{"eng"}},
			Resources: []k8sruntime.RawExtension{{Raw: []byte(
				`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"owner":"${principal.id}"}}`)}},
		},
	}
	require.NoError(t, kube.Client.Create(t.Context(), template))

	t.Setenv("TOOLGATE_LEASE_NAMESPACE", serveLeaseNamespace)
	t.Setenv("TOOLGATE_CATALOG_NAMESPACES", serveCatalogNamespace)
	t.Setenv("TOOLGATE_GATEWAY_POD_LABELS", "app.kubernetes.io/name=toolgate")

	stdout, stderr, done := startServeWith(t, pg.ConnString(), provider)
	defer stopServe(t, stderr, done)

	endpoint := "http://" + gatewayAddress(t, stdout) + gateway.MCPPath

	t.Run("an unauthenticated MCP request is refused", func(t *testing.T) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(`{}`))
		require.NoError(t, err)
		request.Header.Set(auth.SessionHeader, "session-1")

		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)

		_ = response.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	})

	t.Run("toolgate_list_servers offers a loaded template", func(t *testing.T) {
		header := http.Header{}
		header.Set("Authorization", "Bearer "+provider.MintAccessToken(t, testutil.TokenClaims{
			Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"},
		}))
		header.Set(auth.SessionHeader, "session-1")

		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
			Endpoint: endpoint, HTTPClient: &http.Client{Transport: headerTransport{header: header}},
		}, nil)
		require.NoError(t, err)

		defer func() { _ = session.Close() }()

		require.Eventually(t, func() bool {
			names := listServerNames(t, session)

			return len(names) == 1 && names[0] == "notes"
		}, time.Minute, 50*time.Millisecond, "the template is offered; stderr: %s", stderr.String())
	})

	t.Run("the leader writes the template status", func(t *testing.T) {
		require.Eventually(t, func() bool {
			var current v1alpha1.MCPServerTemplate
			if err := kube.Client.Get(t.Context(), types.NamespacedName{Namespace: serveCatalogNamespace, Name: "notes"}, &current); err != nil {
				return false
			}

			return meta.IsStatusConditionTrue(current.Status.Conditions, catalog.ConditionValid)
		}, time.Minute, 50*time.Millisecond)
	})

	t.Run("the leader moves a pending instance to starting", func(t *testing.T) {
		var current v1alpha1.MCPServerTemplate
		require.NoError(t, kube.Client.Get(t.Context(), types.NamespacedName{Namespace: serveCatalogNamespace, Name: "notes"}, &current))

		pool := openPool(t, pg.ConnString())
		queries := db.New(pool)

		const id = "0123456789abcdef0123456789abcdef"

		_, err := queries.InsertInstance(t.Context(), db.InsertInstanceParams{
			ID: id, TemplateNs: current.Namespace, TemplateName: current.Name, TemplateUid: string(current.UID),
			Generation: current.Generation, Scope: current.Spec.Scope, ScopeKey: "user:alice", OwnerPrincipal: "alice",
			Namespace: "tg-" + id,
		})
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			instance, err := queries.GetInstance(t.Context(), id)

			return err == nil && instance.Status == "starting"
		}, time.Minute, 50*time.Millisecond, "stderr: %s", stderr.String())

		var settings corev1.ConfigMap
		require.NoError(t, kube.Client.Get(t.Context(), types.NamespacedName{Namespace: "tg-" + id, Name: "settings"}, &settings))
		assert.Equal(t, "alice", settings.Data["owner"])
	})
}
