package runtime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// silentUpstream accepts every request and never answers it.
func silentUpstream(t *testing.T) *httptest.Server {
	t.Helper()

	stop := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))

	t.Cleanup(func() { close(stop); server.Close() })

	return server
}

func TestStarterToolListTimeoutDefault(t *testing.T) {
	assert.Equal(t, 10*time.Second, runtime.DefaultToolListTimeout)
}

// A starting instance whose server never answers MCP doesn't hold up the
// step: the tool list times out, the instance stays starting, and it fails
// once its startTimeout passes.
func TestStarterStepIsBoundedWhenTheUpstreamNeverAnswers(t *testing.T) {
	server := silentUpstream(t)

	fixture := newStarterFixture(t, testTemplate(), "starting", readyPod())
	fixture.starter.ToolListTimeout = 100 * time.Millisecond
	fixture.starter.ListTools = func(ctx context.Context, _ string) ([]*mcp.Tool, error) {
		return runtime.MCPToolLister(ctx, server.URL)
	}

	step := func() {
		done := make(chan error, 1)

		go func() { done <- fixture.starter.Step(context.Background(), fixture.instance) }()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			require.FailNow(t, "Starter.Step is still blocked on an upstream that never answers")
		}
	}

	step()

	status, _, _, _ := fixture.store.snapshot()
	assert.Equal(t, "starting", status, "a tool list that times out leaves the instance starting")

	fixture.now = fixture.instance.AttemptStartedAt.Time.Add(runtime.DefaultStartTimeout)

	step()

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Contains(t, reason, "didn't become ready within 5m0s")
}

// A ready instance that announces `list_changed` and then never answers
// `tools/list` keeps being health-checked: the re-list times out.
func TestMonitorReListTimesOutAndHealthChecksGoOn(t *testing.T) {
	fixture := newMonitorFixture(t, nil)

	var listing sync.Once

	listed := make(chan struct{})

	fixture.upstream.mu.Lock()
	fixture.upstream.listTools = func(ctx context.Context) ([]*mcp.Tool, error) {
		listing.Do(func() { close(listed) })
		<-ctx.Done()

		return nil, ctx.Err()
	}
	fixture.upstream.mu.Unlock()

	fixture.upstream.announce([]*mcp.Tool{searchTool})

	select {
	case <-listed:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the monitor didn't re-list the tools")
	}

	pings, _, _ := fixture.upstream.state()

	require.Eventually(t, func() bool {
		after, _, _ := fixture.upstream.state()

		return after >= pings+3
	}, 10*time.Second, time.Millisecond, "health checks stopped after a re-list that never answers")

	row, _ := fixture.store.snapshot()
	assert.Equal(t, "ready", row.Status)
}
