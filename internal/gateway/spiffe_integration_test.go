//go:build integration

package gateway_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// A different SVID on an open MCP session doesn't match the session's
// binding, so it gets 404.
func TestDifferentSVIDOnMCPSessionGets404(t *testing.T) {
	domain := testutil.NewSPIFFETrustDomain(t, "allowed.test")
	bundleServer := httptest.NewTLSServer(domain.BundleHandler(t))
	t.Cleanup(bundleServer.Close)

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundleServer.Certificate().Raw}), 0o600))

	fixture := newMCPFixtureWithAuth(t, &auth.Options{SPIFFE: &auth.SPIFFEOptions{
		TrustDomains: []auth.SPIFFETrustDomain{{Name: "allowed.test", BundleEndpointURL: bundleServer.URL, CAFile: caFile}},
	}})

	dev := user{token: domain.MintSVID(t, "/owner/alice/workspace/dev"), session: "session-1"}
	other := user{token: domain.MintSVID(t, "/owner/alice/workspace/other"), session: "session-1"}

	var mcpSessionID string

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		response := fixture.post(t, dev, "", initializeMessage())
		if assert.Equal(collect, http.StatusOK, response.StatusCode) {
			mcpSessionID = response.Header.Get("Mcp-Session-Id")
		}
	}, 10*time.Second, 20*time.Millisecond)
	require.NotEmpty(t, mcpSessionID)

	listTools := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}

	assert.Equal(t, http.StatusNotFound, fixture.post(t, other, mcpSessionID, listTools).StatusCode)
	assert.NotEqual(t, http.StatusNotFound, fixture.post(t, dev, mcpSessionID, listTools).StatusCode)
}
