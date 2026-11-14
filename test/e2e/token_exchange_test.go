//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/test/tokenexchange"
)

// testTokenEndpoint serves the test token endpoint at
// TOOLGATE_E2E_TOKENEXCHANGE_LISTEN, the address of the chart's token
// exchange `test`. It skips the test when the harness configured none.
func testTokenEndpoint(t *testing.T, lifetime time.Duration) *tokenexchange.Endpoint {
	t.Helper()

	address := os.Getenv("TOOLGATE_E2E_TOKENEXCHANGE_LISTEN")
	if address == "" {
		t.Skip("TOOLGATE_E2E_TOKENEXCHANGE_LISTEN is not set: no test token endpoint (run through scripts/e2e/run.sh)")
	}

	certificate, err := tls.LoadX509KeyPair(os.Getenv("TOOLGATE_E2E_RESOLVER_CERT"), os.Getenv("TOOLGATE_E2E_RESOLVER_KEY"))
	require.NoError(t, err)

	listener, err := net.Listen("tcp", address)
	require.NoError(t, err)

	handler := tokenexchange.New(lifetime)
	mux := http.NewServeMux()
	mux.Handle("POST /token", handler)

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12},
	}

	go func() { _ = server.ServeTLS(listener, "", "") }()

	t.Cleanup(func() { _ = server.Close() })

	return handler
}

// exchangeResources is the per-user test server with a Secret whose only key
// is ${exchange:test}, mounted at /secrets, where the server's secrets tool
// reads it again on each call.
func exchangeResources(t *testing.T, image string) []k8sruntime.RawExtension {
	t.Helper()

	resources := testServerResourcesWithTools(t, image, []string{"echo", "secrets"}, 0, nil)

	var deployment map[string]any
	require.NoError(t, json.Unmarshal(resources[0].Raw, &deployment))

	podSpec := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	podSpec["volumes"] = []any{map[string]any{"name": "credentials", "secret": map[string]any{"secretName": "creds"}}}
	container := podSpec["containers"].([]any)[0].(map[string]any)
	container["volumeMounts"] = []any{map[string]any{"name": "credentials", "mountPath": "/secrets", "readOnly": true}}

	raw, err := json.Marshal(deployment)
	require.NoError(t, err)

	resources[0].Raw = raw

	secret, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "creds"},
		"stringData": map[string]any{"token": "${exchange:test}"},
	})
	require.NoError(t, err)

	return append(resources, k8sruntime.RawExtension{Raw: secret})
}

// exchangeTools are the prefixed tools of the server exchangeResources runs.
func exchangeTools(server string) []string {
	return []string{server + "__echo", server + "__secrets"}
}

// instanceIDs returns the instance IDs of the live instance namespaces of a
// template.
func instanceIDs(t require.TestingT, admin *kubernetes.Clientset, template string) []string {
	var ids []string

	for _, namespace := range instanceNamespaces(t, context.Background(), admin, template) {
		ids = append(ids, namespace.Labels[runtime.LabelInstance])
	}

	return ids
}

// exchangedToken calls the secrets tool of the server as session and returns
// the token its pod reads.
func exchangedToken(t require.TestingT, session *gatewaySession, server string) string {
	// One attempt is bounded, so a call that never answers fails the
	// attempt and the caller's wait tries again.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	response, err := session.session.CallTool(ctx, &mcp.CallToolParams{Name: server + "__secrets"})
	require.NoError(t, err)
	require.False(t, response.IsError, "%v", response.Content)

	encoded, err := json.Marshal(response.StructuredContent)
	require.NoError(t, err)

	var files map[string]string

	require.NoError(t, json.Unmarshal(encoded, &files))

	return files["token"]
}

// A per-user server whose Secret holds ${exchange:test} gets its own user's
// exchanged token, a refreshed one while the user keeps working and the same
// session lives, a new instance with a new exchange after the token expired
// unused, and no instance for a user the token endpoint refuses.
func TestTokenExchangeEndToEnd(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	// The shortest lifetime the gateway accepts, so the half-life refresh
	// and the expiry both happen within the test.
	lifetime := credentials.MinExchangedTokenLifetime + 10*time.Second
	endpoint := testTokenEndpoint(t, lifetime)
	endpoint.Refuse("carol", "invalid_grant")

	name := "e2e-exchange-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "per-user",
			Start:        catalog.StartOnEnable,
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       v1alpha1.Access{Users: []string{"alice", "bob", "carol"}, Environments: "any"},
			Endpoint:     v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources:    exchangeResources(t, image),
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })
	requireTemplateValid(t, kube, name)

	gateway := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	alice := connectGateway(t, gateway, "alice", "e2e-"+rand.String(8))
	bob := connectGateway(t, gateway, "bob", "e2e-"+rand.String(8))
	carol := connectGateway(t, gateway, "carol", "e2e-"+rand.String(8))

	var aliceInstance string

	t.Run("two users get instances with their own tokens", func(t *testing.T) {
		require.ElementsMatch(t, exchangeTools(name), enable(t, alice, name))
		aliceInstance = requireOneInstance(t, admin, name).Labels[runtime.LabelInstance]

		require.ElementsMatch(t, exchangeTools(name), enable(t, bob, name))
		require.Len(t, instanceIDs(t, admin, name), 2)

		assert.Equal(t, tokenexchange.Token("alice", 1), exchangedToken(t, alice, name))
		assert.Equal(t, tokenexchange.Token("bob", 1), exchangedToken(t, bob, name))
	})

	t.Run("a refused user gets no instance and the reason names the exchange", func(t *testing.T) {
		result := awaitEnable(t, startEnable(t, carol, name))
		assert.Contains(t, result.err, "Token exchange `test` failed", "tools %v", result.tools)
		assert.Contains(t, result.err, "invalid_grant")
		assert.Len(t, instanceIDs(t, admin, name), 2, "carol has no instance")
	})

	t.Run("the token is refreshed in place while the user keeps working", func(t *testing.T) {
		// A new instance would also have a second token, so the check
		// that the instance stays is part of the wait.
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Contains(collect, instanceIDs(collect, admin, name), aliceInstance, "the instance is not replaced")
			assert.Equal(collect, tokenexchange.Token("alice", 2), exchangedToken(t, alice, name))
		}, enableTimeout, 3*time.Second, "the Secret gets the second token before the first expires")

		assert.Subset(t, listedTools(t, t.Context(), alice.session), exchangeTools(name), "the session survives")
	})

	t.Run("an idle instance stops at the token's expiry and the next call starts it again", func(t *testing.T) {
		// Nobody calls, so nothing refreshes the token and the instance
		// stops when it expires.
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.NotContains(collect, instanceIDs(collect, admin, name), aliceInstance, "alice's instance is gone")
		}, enableTimeout, 3*time.Second)

		var token string

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			token = exchangedToken(collect, alice, name)
			assert.NotEmpty(collect, token)
		}, enableTimeout, 3*time.Second, "the call starts a new instance")

		assert.NotContains(t, []string{tokenexchange.Token("alice", 1), tokenexchange.Token("alice", 2)}, token, "the new instance exchanged again")
		assert.Contains(t, token, "exchanged-alice-")
		assert.NotContains(t, instanceIDs(t, admin, name), aliceInstance, "the new instance is another one")
	})
}
