//go:build e2e

package e2e

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/test/resolver"
)

// testResolver serves the test resolver at TOOLGATE_E2E_RESOLVER_LISTEN, the
// address of the chart's resolver `test`. It skips the test when the
// harness configured no resolver.
func testResolver(t *testing.T, lifetime time.Duration) *resolver.Resolver {
	t.Helper()

	address := os.Getenv("TOOLGATE_E2E_RESOLVER_LISTEN")
	if address == "" {
		t.Skip("TOOLGATE_E2E_RESOLVER_LISTEN is not set: no test resolver (run through scripts/e2e/run.sh)")
	}

	certificate, err := tls.LoadX509KeyPair(os.Getenv("TOOLGATE_E2E_RESOLVER_CERT"), os.Getenv("TOOLGATE_E2E_RESOLVER_KEY"))
	require.NoError(t, err)

	listener, err := net.Listen("tcp", address)
	require.NoError(t, err)

	handler := resolver.New(lifetime)
	mux := http.NewServeMux()
	mux.Handle("POST /resolve", handler)

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12},
	}

	go func() { _ = server.ServeTLS(listener, "", "") }()

	t.Cleanup(func() { _ = server.Close() })

	return handler
}

// resolverServerResources is the test server with a Secret holding
// ${resolver:test}, passed to the server as RESOLVED.
func resolverServerResources(t *testing.T, image string) []k8sruntime.RawExtension {
	t.Helper()

	resources := testServerResources(t, image, 0, nil)

	var deployment map[string]any
	require.NoError(t, json.Unmarshal(resources[0].Raw, &deployment))

	containers := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	container := containers[0].(map[string]any)
	container["env"] = append(container["env"].([]any), map[string]any{
		"name":      "RESOLVED",
		"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "resolved", "key": "value"}},
	})

	raw, err := json.Marshal(deployment)
	require.NoError(t, err)

	resources[0].Raw = raw

	secret, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "resolved"},
		"stringData": map[string]any{"value": "${resolver:test}"},
	})
	require.NoError(t, err)

	return append(resources, k8sruntime.RawExtension{Raw: secret})
}

func TestAC2_18Resolver(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	// The first value lives just over the scaled minimum, plus time for the
	// instance to start and the test to read the value before the
	// reconciler replaces it, a scaled expiry margin before it expires.
	lifetime := scaled(runtime.MinResolvedLifetime) + 45*time.Second
	handler := testResolver(t, lifetime)

	name := createTemplate(t, kube, "e2e-resolver", "per-user", resolverServerResources(t, image))
	requireTemplateValid(t, kube, name)

	t.Run("[AC-2-18] the test server gets the resolver's value, and a fresh one after it expires", func(t *testing.T) {
		pods := gatewayPods(t, admin)
		session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))
		require.ElementsMatch(t, prefixedTools(name), enable(t, session, name))

		old := requireOneInstance(t, admin, name)
		oldID := old.Labels[runtime.LabelInstance]

		var environment map[string]string

		structuredResult(t, session.session, name+"__env", &environment)
		first := environment["RESOLVED"]
		require.NotEmpty(t, first, "the server got the resolver's value")

		requests := handler.Requests()
		require.NotEmpty(t, requests)
		assert.Equal(t, "test", requests[0].Body["resolver"])
		assert.Equal(t, oldID, requests[0].Body["instance"])

		// Later values live long, so the replacement isn't replaced again.
		handler.SetLifetime(time.Hour)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			namespaces := instanceNamespaces(collect, t.Context(), admin, name)
			require.Len(collect, namespaces, 1)
			assert.NotEqual(collect, oldID, namespaces[0].Labels[runtime.LabelInstance])
		}, lifetime+enableTimeout, time.Second, "the instance is replaced before its value expires")

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Subset(collect, listedTools(collect, t.Context(), session.session), prefixedTools(name))
		}, enableTimeout, time.Second, "the session lists the server's tools again")

		structuredResult(t, session.session, name+"__env", &environment)
		assert.NotEmpty(t, environment["RESOLVED"])
		assert.NotEqual(t, first, environment["RESOLVED"], "the new instance has a fresh value")
	})
}
