//go:build integration

package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// secretValue is the value the test stores. It is distinctive so a search of
// objects, logs and audit rows finds every copy.
const secretValue = "ghp-value-7c0ffee-only-in-a-secret"

// servedGateway is a running `toolgate serve` and what it runs against.
type servedGateway struct {
	provider    *testutil.OIDCProvider
	kube        *testutil.Envtest
	databaseURL string
	url         string
	stdout      *lockedBuffer
	stderr      *lockedBuffer
	done        chan int
}

// startServeWithSecrets starts serve with PostgreSQL, envtest, the fake IdP
// and a secrets keyset, with one template in the catalog that uses
// ${secret:github-token} in a Secret.
func startServeWithSecrets(t *testing.T) servedGateway {
	t.Helper()

	return startServeWithAudiences(t)
}

// startServeWithAudiences is startServeWithSecrets with extra accepted
// audiences next to the fake IdP's own.
func startServeWithAudiences(t *testing.T, extraAudiences ...string) servedGateway {
	t.Helper()
	clearConfigEnv(t)

	pg := testutil.StartPostgres(t)
	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	kube := testutil.StartEnvtest(t)
	writeKubeconfig(t, kube)

	for _, name := range []string{serveLeaseNamespace, serveCatalogNamespace} {
		require.NoError(t, kube.Client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}

	require.NoError(t, kube.Client.Create(t.Context(), &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: serveCatalogNamespace, Name: "github"},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Description: "GitHub tools",
			Scope:       catalog.ScopePerUser,
			Namespace:   catalog.NamespacePerInstance,
			Endpoint:    v1alpha1.Endpoint{Service: "mcp", Port: 8080},
			Access:      v1alpha1.Access{Groups: []string{"eng"}, Environments: policy.EnvironmentAny},
			Resources: []k8sruntime.RawExtension{
				{Raw: []byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"github"},"stringData":{"token":"${secret:github-token}"}}`)},
				{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"owner":"${principal.id}"}}`)},
			},
		},
	}))

	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)

	var buffer bytes.Buffer
	require.NoError(t, insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buffer)))

	keysetPath := filepath.Join(t.TempDir(), "keyset.json")
	require.NoError(t, os.WriteFile(keysetPath, buffer.Bytes(), 0o600))

	t.Setenv("TOOLGATE_SECRETS_KEYSET_FILE", keysetPath)

	if len(extraAudiences) > 0 {
		t.Setenv("TOOLGATE_OIDC_AUDIENCE", strings.Join(append([]string{provider.Audience}, extraAudiences...), ","))
	}

	t.Setenv("TOOLGATE_OIDC_CLI_CLIENT_ID", provider.ClientID)
	t.Setenv("TOOLGATE_LEASE_NAMESPACE", serveLeaseNamespace)
	t.Setenv("TOOLGATE_CATALOG_NAMESPACES", serveCatalogNamespace)
	t.Setenv("TOOLGATE_GATEWAY_POD_LABELS", "app.kubernetes.io/name=toolgate")

	stdout, stderr, done := startServeWith(t, pg.ConnString(), provider)

	return servedGateway{
		provider: provider, kube: kube, databaseURL: pg.ConnString(),
		url: "http://" + gatewayAddress(t, stdout), stdout: stdout, stderr: stderr, done: done,
	}
}

// connectAs opens an MCP session to the gateway as alice with the identity
// headers.
func (g servedGateway) connectAs(t *testing.T, session, workspace string) *mcp.ClientSession {
	t.Helper()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+g.provider.MintAccessToken(t, testutil.TokenClaims{
		Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"},
	}))
	header.Set(auth.SessionHeader, session)

	if workspace != "" {
		header.Set(auth.WorkspaceHeader, workspace)
	}

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	clientSession, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: g.url + gateway.MCPPath, HTTPClient: &http.Client{Transport: headerTransport{header: header}},
	}, nil)
	require.NoError(t, err)

	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

// objectsHolding returns "Kind namespace/name" for every object of the
// listed core kinds whose content holds value.
func objectsHolding(t *testing.T, kube client.Client, value string) []string {
	t.Helper()

	var found []string

	var secretList corev1.SecretList
	require.NoError(t, kube.List(t.Context(), &secretList))

	for _, item := range secretList.Items {
		for _, data := range item.Data {
			if strings.Contains(string(data), value) {
				found = append(found, "Secret "+item.Namespace+"/"+item.Name)
			}
		}
	}

	for _, kind := range []string{"ConfigMapList", "ServiceAccountList", "ServiceList", "NamespaceList"} {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("v1")
		list.SetKind(kind)
		require.NoError(t, kube.List(t.Context(), list))

		for _, item := range list.Items {
			encoded, err := item.MarshalJSON()
			require.NoError(t, err)

			if strings.Contains(string(encoded), value) {
				found = append(found, item.GetKind()+" "+item.GetNamespace()+"/"+item.GetName())
			}
		}
	}

	return found
}

func TestAC1_21SecretCommands(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("TOOLGATE_SESSION", "session-cli")
	t.Setenv("TOOLGATE_WORKSPACE", "laptop-widgets")

	served := startServeWithSecrets(t)
	defer stopServe(t, served.stderr, served.done)

	served.provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"}})

	code, _, signInStderr := signInApproving(t, served.provider, "alice", "--gateway", served.url)
	require.Equal(t, 0, code, "stderr: %s", signInStderr)

	t.Run("[AC-1-21] toolgate secret set reads the value from stdin and never echoes it", func(t *testing.T) {
		code, out, errOut := runCLI(t, secretValue, "secret", "set", "github-token")
		require.Equal(t, 0, code, "stderr: %s", errOut)
		assert.Contains(t, out, "github-token")
		assert.NotContains(t, out+errOut, secretValue)
	})

	t.Run("[AC-1-21] toolgate secret list shows the name and dates only", func(t *testing.T) {
		code, out, errOut := runCLI(t, "", "secret", "list")
		require.Equal(t, 0, code, "stderr: %s", errOut)
		assert.NotContains(t, out, secretValue)

		lines := strings.Split(strings.TrimSpace(out), "\n")
		require.Len(t, lines, 2, "a header and one secret: %q", out)
		assert.Regexp(t, `^NAME\s+CREATED\s+UPDATED$`, lines[0])

		date := `\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`
		assert.Regexp(t, regexp.MustCompile(`^github-token\s+`+date+`\s+`+date+`$`), lines[1])
	})

	t.Run("toolgate servers lists what toolgate_list_servers returns for the CLI user", func(t *testing.T) {
		session := served.connectAs(t, "session-cli", "laptop-widgets")

		require.Eventually(t, func() bool {
			names := listServerNames(t, session)

			return len(names) == 1 && names[0] == "github"
		}, time.Minute, 50*time.Millisecond, "the template is offered; stderr: %s", served.stderr.String())

		code, out, errOut := runCLI(t, "", "servers")
		require.Equal(t, 0, code, "stderr: %s", errOut)

		lines := strings.Split(strings.TrimSpace(out), "\n")
		require.Len(t, lines, 2, "a header and one server: %q", out)
		assert.Regexp(t, `^NAME\s+SCOPE\s+STATUS\s+ENABLED\s+DESCRIPTION$`, lines[0])
		assert.Regexp(t, `^github\s+per-user\s+stopped\s+false\s+GitHub tools$`, lines[1])
	})

	t.Run("[AC-1-21] enabling the template gives the value to the instance through a Secret object only", func(t *testing.T) {
		session := served.connectAs(t, "session-enable", "")

		// envtest runs no pods, so the instance never becomes ready: the
		// enable waits until the test has seen the Secret and disables the
		// server. Cancelling the call's context would not do: the client
		// sends notifications/cancelled in the background, closing the
		// session can drop it, and the gateway then keeps waiting and holds
		// up its shutdown. The disable ends the wait on the gateway, and
		// the enable's reply proves it ended.
		enableDone := make(chan struct{})

		go func() {
			defer close(enableDone)

			_, _ = session.CallTool(t.Context(), &mcp.CallToolParams{Name: gateway.ToolEnableServer, Arguments: map[string]any{"name": "github"}})
		}()

		defer func() {
			_, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: gateway.ToolDisableServer, Arguments: map[string]any{"name": "github"}})
			assert.NoError(t, err, "disable the server to end the enable")

			select {
			case <-enableDone:
			case <-time.After(time.Minute):
				assert.Fail(t, "the enable did not end after the disable", "stderr: %s", served.stderr.String())
			}
		}()

		var holders []string

		require.Eventually(t, func() bool {
			holders = objectsHolding(t, served.kube.Client, secretValue)

			return len(holders) > 0
		}, time.Minute, 100*time.Millisecond, "the rendered Secret appears; stderr: %s", served.stderr.String())

		require.Len(t, holders, 1, "only one object holds the value: %v", holders)
		assert.Regexp(t, `^Secret tg-[0-9a-f]+/github$`, holders[0])

		assert.NotContains(t, served.stdout.String(), secretValue, "serve's stdout (logs and audit lines)")
		assert.NotContains(t, served.stderr.String(), secretValue, "serve's stderr")

		var auditHits int
		require.NoError(t, openPool(t, served.databaseURL).QueryRow(t.Context(),
			"SELECT count(*) FROM audit_events WHERE strpos(audit_events::text, $1) > 0", secretValue).Scan(&auditHits))
		assert.Zero(t, auditHits, "no audit event holds the value")
	})

	t.Run("toolgate secret delete removes the secret", func(t *testing.T) {
		code, _, errOut := runCLI(t, "", "secret", "delete", "github-token")
		require.Equal(t, 0, code, "stderr: %s", errOut)

		code, out, errOut := runCLI(t, "", "secret", "list")
		require.Equal(t, 0, code, "stderr: %s", errOut)
		assert.NotContains(t, out, "github-token")
	})
}
