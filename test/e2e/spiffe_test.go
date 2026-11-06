//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// spiffeBundleEndpoint serves the bundle of allowed.test at
// TOOLGATE_E2E_SPIFFE_LISTEN, the chart's bundle endpoint for that trust
// domain, with the test resolver's certificate. It skips the test when the
// harness configured no trust domain.
func spiffeBundleEndpoint(t *testing.T, domain *testutil.SPIFFETrustDomain) {
	t.Helper()

	address := os.Getenv("TOOLGATE_E2E_SPIFFE_LISTEN")
	if address == "" {
		t.Skip("TOOLGATE_E2E_SPIFFE_LISTEN is not set: no SPIFFE bundle endpoint (run through scripts/e2e/run.sh)")
	}

	certificate, err := tls.LoadX509KeyPair(os.Getenv("TOOLGATE_E2E_RESOLVER_CERT"), os.Getenv("TOOLGATE_E2E_RESOLVER_KEY"))
	require.NoError(t, err)

	listener, err := net.Listen("tcp", address)
	require.NoError(t, err)

	server := &http.Server{
		Handler:           domain.BundleHandler(t),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12},
	}

	go func() { _ = server.ServeTLS(listener, "", "") }()

	t.Cleanup(func() { _ = server.Close() })
}

// initializeStatus sends an MCP initialize request with token and returns
// the response status.
func initializeStatus(t require.TestingT, ctx context.Context, endpoint, token string) int {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "toolgate-e2e", "version": "0"},
		},
	})
	require.NoError(t, err)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set(auth.SessionHeader, "e2e-"+rand.String(8))

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	return response.StatusCode
}

func serverEntry(t require.TestingT, ctx context.Context, session *mcp.ClientSession, name string) (gateway.ServerEntry, bool) {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: gateway.ToolListServers})
	require.NoError(t, err)
	require.False(t, result.IsError, "list_servers failed: %v", result.Content)

	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)

	var decoded gateway.ListServersResult
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	for _, entry := range decoded.Servers {
		if entry.Name == name {
			return entry, true
		}
	}

	return gateway.ServerEntry{}, false
}

func TestAC3_09SPIFFE(t *testing.T) {
	allowed := testutil.NewSPIFFETrustDomain(t, "allowed.test")
	other := testutil.NewSPIFFETrustDomain(t, "other.test")
	spiffeBundleEndpoint(t, allowed)

	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	// An attested-only template that only allowed.test's owners may use.
	name := "e2e-spiffe-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "per-session",
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       v1alpha1.Access{SPIFFEIDs: []string{"spiffe://allowed.test/owner/*"}, Environments: "attested"},
			Endpoint:     v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources:    testServerResources(t, image, 0, nil),
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })
	requireTemplateValid(t, kube, name)

	// A rule refusing the server in scratch workspaces only.
	rule := &v1alpha1.ToolgateRule{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: "e2e-spiffe-rule-" + rand.String(5)},
		Spec: v1alpha1.ToolgateRuleSpec{
			Match: v1alpha1.RuleMatch{
				Workspaces: []string{"/owner/*/workspace/scratch-*"}, Attestation: "attested", Servers: []string{name},
			},
			Reason: "No scratch workspaces.",
		},
	}
	require.NoError(t, kube.Create(t.Context(), rule))
	t.Cleanup(func() {
		_ = kube.Delete(context.WithoutCancel(t.Context()), rule, client.PropagationPolicy(metav1.DeletePropagationBackground))
	})

	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	devToken := allowed.MintSVID(t, "/owner/alice/workspace/dev")

	// The gateway retries a failed bundle fetch at most 30 s apart, so the
	// bundle arrives soon after the endpoint starts.
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Equal(collect, http.StatusOK, initializeStatus(collect, t.Context(), endpoint, devToken))
	}, 2*time.Minute, time.Second)

	t.Run("[AC-3-09] an SVID from the allowed trust domain is attested", func(t *testing.T) {
		session := connectWithToken(t, endpoint, devToken, "e2e-"+rand.String(8))

		entry, found := serverEntry(t, t.Context(), session.session, name)
		require.True(t, found, "spiffeIDs gives the SVID access")
		assert.NotEqual(t, gateway.StatusBlocked, entry.Status, "an attested-only server passes without a grant: %s", entry.Reason)
	})

	t.Run("[AC-3-09] the workspace is the SPIFFE ID's path and a rule matches it", func(t *testing.T) {
		scratch := connectWithToken(t, endpoint, allowed.MintSVID(t, "/owner/alice/workspace/scratch-1"), "e2e-"+rand.String(8))

		// The rule reaches the gateway's cache through a watch.
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			entry, found := serverEntry(collect, t.Context(), scratch.session, name)
			assert.True(collect, found)
			assert.Equal(collect, gateway.StatusBlocked, entry.Status)
			assert.True(collect, strings.Contains(entry.Reason, "No scratch workspaces."), entry.Reason)
		}, time.Minute, time.Second)
	})

	t.Run("[AC-3-09] an SVID from another trust domain gets 401", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, initializeStatus(t, t.Context(), endpoint, other.MintSVID(t, "/owner/alice/workspace/dev")))
	})
}
