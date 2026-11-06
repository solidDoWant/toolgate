package auth_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/opsmetrics"
	tgtestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

func authResult(kind, result string) float64 {
	return opsmetrics.Value("toolgate_auth_results_total", map[string]string{"kind": kind, "result": result})
}

// Each sign-in counts once, by the kind of client and its result.
func TestAuthResultsAreCounted(t *testing.T) {
	// Not parallel: the counters are process-wide.
	web := newFixture(t, auth.Options{}, false)
	mcp := newFixture(t, auth.Options{}, true)

	token := func(provider *tgtestutil.OIDCProvider) string {
		return provider.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice", ExpiresAt: time.Now().Add(time.Hour)})
	}

	userOK, userRefused := authResult("user", "ok"), authResult("user", "refused")
	connectorOK, connectorRefused := authResult("connector", "ok"), authResult("connector", "refused")

	assert.Equal(t, http.StatusOK, web.do(t, token(web.provider), map[string]string{"X-Toolgate-Session": "s1"}).Code)
	assert.Equal(t, http.StatusUnauthorized, web.do(t, "", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, web.do(t, "garbage", nil).Code)

	assert.InDelta(t, userOK+1, authResult("user", "ok"), 0)
	assert.InDelta(t, userRefused+2, authResult("user", "refused"), 0)

	// On the MCP endpoint a user without a session header is a connector.
	assert.Equal(t, http.StatusOK, mcp.do(t, token(mcp.provider), nil).Code)
	assert.Equal(t, http.StatusUnauthorized, mcp.do(t, "", nil).Code)
	assert.Equal(t, http.StatusOK, mcp.do(t, token(mcp.provider), map[string]string{"X-Toolgate-Session": "s2"}).Code)

	assert.InDelta(t, connectorOK+1, authResult("connector", "ok"), 0)
	assert.InDelta(t, connectorRefused+1, authResult("connector", "refused"), 0)
	assert.InDelta(t, userOK+2, authResult("user", "ok"), 0)
}
