package runtime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// An upstream that accepts the connection and never answers initialize.
func TestBughuntHangingUpstreamBlocksStarterStep(t *testing.T) {
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); hang.CloseClientConnections(); hang.Close() })

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instanceNamespace, Labels: instanceLabels()},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}
	fixture := newStarterFixture(t, testTemplate(), "starting", readyPod(), service)
	fixture.starter.ListTools = func(ctx context.Context, _ string) ([]*mcp.Tool, error) {
		return runtime.MCPToolLister(ctx, hang.URL)
	}

	// The reconcile context has no deadline, like controller-runtime's.
	done := make(chan error, 1)
	go func() { done <- fixture.starter.Step(context.Background(), fixture.instance) }()

	select {
	case err := <-done:
		t.Logf("Step returned: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("Starter.Step is still blocked after 60 s on an upstream that never answers; the full pass is sequential, so every other instance waits too")
	}
}
