//go:build integration

package gateway_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	toolruntime "github.com/solidDoWant/toolgate/internal/runtime"
)

func TestSearchQualityInUse(t *testing.T) {
	t.Run("a call at rank 3 is a hit, a replaced search a re-search, an idle search no call, and no row holds the query", func(t *testing.T) {
		metrics := toolruntime.NewMetrics()
		fixture := newMCPFixture(t, func(options *gateway.MCPOptions) { options.Metrics = metrics })
		template := fixture.addEnableTemplate("github", "per-user", anyone)
		fixture.declareTools(t, template,
			declared("echo_text", "Echo text back to the caller"),
			declared("echo_lines", "Echo lines of text back"),
			declared("echo_words", "Echo words of text back"),
			declared("echo_all", "Echo everything of text back"))
		fixture.startUpstream(t, "github")

		client := fixture.connect(t, searching(alice()))

		const secret = "zebrasecret"

		// A search, then a call of the tool at rank 3.
		found, _ := findTools(t, client, "echo text "+secret, 0)
		require.GreaterOrEqual(t, len(found.Tools), 3)

		third := found.Tools[2].Name

		outcomes := make(chan *mcp.CallToolResult, 1)

		go func() {
			result, _ := client.CallTool(t.Context(), &mcp.CallToolParams{Name: third, Arguments: map[string]any{"text": "x"}})
			outcomes <- result
		}()

		instance := fixture.awaitInstance(t, template, "user:alice")
		fixture.markReady(t, instance.ID, "echo_text", "echo_lines", "echo_words", "echo_all")

		<-outcomes

		hits := fixture.auditRows(t, "tool.search_hit")
		require.Len(t, hits, 1)
		assert.EqualValues(t, 3, hits[0].Details["rank"])
		assert.Equal(t, strings.TrimPrefix(third, "github__"), hits[0].Tool)
		assert.Equal(t, 1.0, promtestutil.ToFloat64(metrics.SearchHits.WithLabelValues("3", "false", "")))

		// A second call of the same search is not a second hit.
		go func() {
			result, _ := client.CallTool(t.Context(), &mcp.CallToolParams{Name: third, Arguments: map[string]any{"text": "x"}})
			outcomes <- result
		}()

		<-outcomes

		assert.Len(t, fixture.auditRows(t, "tool.search_hit"), 1)

		// Two searches with no call between them: one re-search.
		findTools(t, client, "echo lines "+secret, 0)
		assert.Equal(t, 0.0, promtestutil.ToFloat64(metrics.SearchResearches.WithLabelValues("false", "")))
		findTools(t, client, "echo words "+secret, 0)
		assert.Equal(t, 1.0, promtestutil.ToFloat64(metrics.SearchResearches.WithLabelValues("false", "")))

		// The last search is idle: after the cutoff it counts once as no call.
		ender := &toolruntime.SessionEnder{Pool: fixture.pool, Metrics: metrics}
		require.NoError(t, ender.Run(t.Context()))
		assert.Equal(t, 0.0, promtestutil.ToFloat64(metrics.SearchNoCalls.WithLabelValues("false", "")))

		_, err := fixture.pool.Exec(t.Context(), `UPDATE session_open_searches SET searched_at = now() - interval '31 minutes'`)
		require.NoError(t, err)
		require.NoError(t, ender.Run(t.Context()))
		require.NoError(t, ender.Run(t.Context()))
		assert.Equal(t, 1.0, promtestutil.ToFloat64(metrics.SearchNoCalls.WithLabelValues("false", "")))

		assert.Equal(t, 3.0, promtestutil.ToFloat64(metrics.Searches.WithLabelValues("false", "")))

		searches := fixture.auditRows(t, "tool.search")
		require.Len(t, searches, 3)

		for _, row := range append(searches, hits...) {
			encoded, err := json.Marshal(row.Details)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), secret)
			assert.NotContains(t, string(encoded), "query")
		}

		assert.EqualValues(t, len(found.Tools), searches[0].Details["count"])
		assert.NotEmpty(t, searches[0].Details["search_id"])
	})
}
