//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	k8srand "k8s.io/apimachinery/pkg/util/rand"
)

// randomHex returns n random bytes as lowercase hex.
func randomHex(t *testing.T, n int) string {
	t.Helper()

	buffer := make([]byte, n)
	_, err := rand.Read(buffer)
	require.NoError(t, err)

	return hex.EncodeToString(buffer)
}

// descendsFrom reports whether span's chain of parents, through the spans
// of its trace, reaches the span with ID ancestor.
func descendsFrom(span *tracepb.Span, byID map[string]*tracepb.Span, ancestor string) bool {
	for range len(byID) + 1 {
		parent := hex.EncodeToString(span.GetParentSpanId())
		if parent == ancestor {
			return true
		}

		next, found := byID[parent]
		if !found {
			return false
		}

		span = next
	}

	return false
}

func TestTraces(t *testing.T) {
	receiver := otlpReceiver(t)
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	// A new template, so the enable inserts a fresh instance and the
	// reconciler renders and applies it.
	name := createTemplate(t, kube, "e2e-traces", "per-session", testServerResources(t, image, 0, nil))
	requireTemplateValid(t, kube, name)

	traceID, parentSpanID := randomHex(t, 16), randomHex(t, 8)
	header := http.Header{}
	header.Set("Traceparent", "00-"+traceID+"-"+parentSpanID+"-01")

	pods := gatewayPods(t, admin)
	session := connectGatewayWithHeader(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+k8srand.String(8), header)

	enable(t, session, name)

	result, err := session.session.CallTool(t.Context(), &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": "traced"}})
	require.NoError(t, err)
	require.False(t, result.IsError, "the call failed: %v", result.Content)

	t.Run("the policy, render, apply and forward spans descend from the request's traceparent", func(t *testing.T) {
		wanted := []string{"toolgate.policy", "toolgate.render", "toolgate.apply", "toolgate.forward"}

		// The gateway exports spans in batches, every few seconds.
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			byID := map[string]*tracepb.Span{}

			for _, span := range receiver.Spans() {
				if hex.EncodeToString(span.GetTraceId()) == traceID {
					byID[hex.EncodeToString(span.GetSpanId())] = span
				}
			}

			for _, spanName := range wanted {
				var found, linked bool

				for _, span := range byID {
					if span.GetName() != spanName {
						continue
					}

					found = true
					linked = linked || descendsFrom(span, byID, parentSpanID)
				}

				assert.True(collect, found, "no %s span in trace %s", spanName, traceID)
				assert.True(collect, linked, "no %s span descends from span %s", spanName, parentSpanID)
			}
		}, 2*time.Minute, time.Second)
	})
}
