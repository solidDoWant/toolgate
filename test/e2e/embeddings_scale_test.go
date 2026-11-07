//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
)

const embeddingsComponentSelector = "app.kubernetes.io/component=embeddings"

// embeddingsReplicas returns the desired and ready replicas of the bundled
// embeddings server's Deployment.
func embeddingsReplicas(ctx context.Context, t require.TestingT, admin *kubernetes.Clientset) (desired, ready int32) {
	deployments, err := admin.AppsV1().Deployments(releaseNamespace()).List(ctx, metav1.ListOptions{LabelSelector: embeddingsComponentSelector})
	require.NoError(t, err)
	require.Len(t, deployments.Items, 1)

	deployment := &deployments.Items[0]
	if deployment.Spec.Replicas == nil {
		return 1, deployment.Status.ReadyReplicas
	}

	return *deployment.Spec.Replicas, deployment.Status.ReadyReplicas
}

// searchEmbeddingErrors returns, summed over the gateway pods, the value of
// toolgate_tool_search_embedding_errors_total for use=query and a reason.
func searchEmbeddingErrors(t *testing.T, admin *kubernetes.Clientset, reason string) float64 {
	t.Helper()

	var total float64

	for _, pod := range gatewayPods(t, admin) {
		body, err := admin.CoreV1().Pods(releaseNamespace()).ProxyGet("http", pod, "9090", "metrics", nil).DoRaw(t.Context())
		require.NoError(t, err)

		prefix := `toolgate_tool_search_embedding_errors_total{reason="` + reason + `",use="query"} `

		scanner := bufio.NewScanner(bytes.NewReader(body))
		for scanner.Scan() {
			if value, found := strings.CutPrefix(scanner.Text(), prefix); found {
				number, err := strconv.ParseFloat(value, 64)
				require.NoError(t, err)

				total += number
			}
		}
	}

	return total
}

func searchTools(t *testing.T, session *gatewaySession, query string) gateway.FindToolsResult {
	t.Helper()

	result, err := session.session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: gateway.ToolFindTools, Arguments: map[string]any{"query": query, "limit": 20},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, toolText(result))

	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)

	var found gateway.FindToolsResult

	require.NoError(t, json.Unmarshal(encoded, &found))

	return found
}

func TestEmbeddingsServerScalesWithToolSearch(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])

	idle := scaled(time.Minute)
	deadline := idle + 3*time.Minute

	template := createTemplate(t, kube, "e2e-embed", "per-session", testServerResources(t, mcpServerImage(t), 0, nil))
	declareTools(t, template, "", "echo")

	toolSearch := http.Header{}
	toolSearch.Set(auth.ToolSearchHeader, "on")

	requireDown := func(t *testing.T) {
		t.Helper()

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			desired, _ := embeddingsReplicas(t.Context(), collect, admin)
			assert.Zero(collect, desired, "the embeddings server is scaled down once tool search is idle")
		}, deadline, time.Second)
	}

	// Adding the template makes the leader embed its tools, which starts the
	// server; wait until the search rows exist and the server is idle again.
	warmID := "e2e-" + rand.String(8)
	warm := connectGatewayWithHeader(t, endpoint, "alice", warmID, toolSearch)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.NotEmpty(collect, searchTools(t, warm, "echo").Tools, "the search rows are built by the leader")
	}, 2*time.Minute, time.Second)
	require.NoError(t, warm.session.Close())
	idleOut(t, admin, warmID)
	requireDown(t)

	t.Run("a search while the server is scaled down uses words only and counts scaled_down, not error", func(t *testing.T) {
		scaledDownBefore := searchEmbeddingErrors(t, admin, "scaled_down")
		errorsBefore := searchEmbeddingErrors(t, admin, "error")

		sessionID := "e2e-" + rand.String(8)
		session := connectGatewayWithHeader(t, endpoint, "alice", sessionID, toolSearch)

		found := searchTools(t, session, "echo")
		assert.NotEmpty(t, found.Tools)
		assert.NotEmpty(t, found.Note, "the note says the search used words only")

		assert.Greater(t, searchEmbeddingErrors(t, admin, "scaled_down"), scaledDownBefore)
		assert.InDelta(t, errorsBefore, searchEmbeddingErrors(t, admin, "error"), 0)

		t.Run("the server starts, and once it is ready a search is not degraded", func(t *testing.T) {
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				desired, _ := embeddingsReplicas(t.Context(), collect, admin)
				assert.EqualValues(collect, 1, desired)
			}, deadline, time.Second)

			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				found := searchTools(t, session, "echo")
				assert.NotEmpty(collect, found.Tools)
				assert.Empty(collect, found.Note, "the search is not degraded")
			}, 3*time.Minute, 2*time.Second)
		})

		t.Run("after the session ends and the idle timeout passes the server is back at zero", func(t *testing.T) {
			require.NoError(t, session.session.Close())
			idleOut(t, admin, sessionID)
			requireDown(t)
		})
	})
}
