//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// metricsAddress returns the metrics port address that serve logged.
func metricsAddress(t *testing.T, stdout *lockedBuffer) string {
	t.Helper()

	for line := range strings.SplitSeq(stdout.String(), "\n") {
		var entry struct{ Msg, MetricsAddress string }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "gateway started" {
			return entry.MetricsAddress
		}
	}

	require.FailNow(t, "serve logged no metrics address")

	return ""
}

func TestServeRecordsRefusedSignIns(t *testing.T) {
	clearConfigEnv(t)

	pg := testutil.StartPostgres(t)
	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	stdout, stderr, done := startServeWith(t, pg.ConnString(), provider)

	address := gatewayAddress(t, stdout)

	tokens := map[string]string{
		"no token":       "",
		"invalid token":  "not.a.token",
		"expired token":  provider.MintAccessToken(t, testutil.TokenClaims{Subject: "alice", IssuedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}),
		"other audience": provider.MintAccessToken(t, testutil.TokenClaims{Subject: "alice", Audience: []string{"another-gateway"}}),
	}

	for name, token := range tokens {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+address+"/mcp", nil)
		require.NoError(t, err)
		request.Header.Set(auth.SessionHeader, "session-1")

		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}

		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)

		_ = response.Body.Close()

		require.Equal(t, http.StatusUnauthorized, response.StatusCode, name)
	}

	pool := openPool(t, pg.ConnString())

	var rows int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT count(*) FROM audit_events WHERE type = $1 AND principal = ''", audit.TypeAuthRefuse).Scan(&rows))
	assert.Equal(t, len(tokens), rows, "each refusal writes one audit_events row")

	var lines int

	for line := range strings.SplitSeq(stdout.String(), "\n") {
		var entry struct{ Msg, Type string }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "audit" && entry.Type == audit.TypeAuthRefuse {
			lines++
		}
	}

	assert.Equal(t, len(tokens), lines, "each refusal writes one audit line to stdout")

	metricsRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+metricsAddress(t, stdout)+"/metrics", nil)
	require.NoError(t, err)

	response, err := http.DefaultClient.Do(metricsRequest)
	require.NoError(t, err)

	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()

	require.NoError(t, err)
	assert.Contains(t, string(body), "toolgate_auth_refusals_dropped_total 0")

	stopServe(t, stderr, done)
}
