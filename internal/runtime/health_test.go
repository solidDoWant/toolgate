package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// healthStore is a HealthStore whose compare-and-set writes behave like the
// SQL ones.
type healthStore struct {
	mu           sync.Mutex
	row          db.Instance
	toolWrite    int
	healthWrites []healthWrite
}

// healthWrite is one change of health the monitor recorded.
type healthWrite struct {
	healthy bool
	reason  string
}

func (h *healthStore) Instance(context.Context, string) (db.Instance, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.row, nil
}

func (h *healthStore) MarkFailed(_ context.Context, _, from, reason string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.row.Status != from {
		return false, nil
	}

	h.row.Status, h.row.Desired, h.row.Reason = "failed", "stopped", reason

	return true, nil
}

func (h *healthStore) SetTools(_ context.Context, _ string, tools []byte) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.row.Status != "ready" {
		return false, nil
	}

	h.row.Tools = tools
	h.toolWrite++

	return true, nil
}

func (h *healthStore) SetHealth(_ context.Context, _ db.Instance, healthy bool, reason string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.row.Status != "ready" || h.row.Desired != "running" || h.row.UnhealthySince.Valid != healthy {
		return false, nil
	}

	h.row.UnhealthySince = pgtype.Timestamptz{Time: time.Now(), Valid: !healthy}
	h.row.UnhealthyReason = reason
	h.healthWrites = append(h.healthWrites, healthWrite{healthy: healthy, reason: reason})

	return true, nil
}

func (h *healthStore) snapshot() (db.Instance, int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.row, h.toolWrite
}

func (h *healthStore) setStatus(status string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.row.Status = status
}

// upstream is a fake monitored server. pingResult decides the outcome of
// ping number n (counted from 1).
type upstream struct {
	mu           sync.Mutex
	pings        int
	pingResult   func(n int) error
	tools        []*mcp.Tool
	listTools    func(ctx context.Context) ([]*mcp.Tool, error)
	toolsChanged func()
	open         bool
	connects     int
	endpoints    []string
}

func (u *upstream) connect(_ context.Context, endpoint string, toolsChanged func()) (runtime.MonitorSession, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.connects++
	u.endpoints = append(u.endpoints, endpoint)
	u.toolsChanged = toolsChanged
	u.open = true

	return upstreamSession{upstream: u}, nil
}

// announce changes the tool list and announces `list_changed`.
func (u *upstream) announce(tools []*mcp.Tool) {
	u.mu.Lock()
	u.tools = tools
	notify := u.toolsChanged
	u.mu.Unlock()

	notify()
}

func (u *upstream) state() (int, bool, int) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.pings, u.open, u.connects
}

type upstreamSession struct{ upstream *upstream }

func (s upstreamSession) Ping(context.Context) error {
	s.upstream.mu.Lock()
	defer s.upstream.mu.Unlock()

	s.upstream.pings++
	if s.upstream.pingResult == nil {
		return nil
	}

	return s.upstream.pingResult(s.upstream.pings)
}

func (s upstreamSession) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	s.upstream.mu.Lock()
	listTools := s.upstream.listTools
	tools := s.upstream.tools
	s.upstream.mu.Unlock()

	if listTools != nil {
		return listTools(ctx)
	}

	return tools, nil
}

func (s upstreamSession) Close() error {
	s.upstream.mu.Lock()
	defer s.upstream.mu.Unlock()

	s.upstream.open = false

	return nil
}

type monitorFixture struct {
	monitor  *runtime.Monitor
	store    *healthStore
	upstream *upstream
	instance db.Instance
	cancel   context.CancelFunc
	done     chan struct{}
}

var searchTool = &mcp.Tool{Name: "search", InputSchema: map[string]any{"type": "object"}}

func encodeTools(t *testing.T, tools ...*mcp.Tool) []byte {
	t.Helper()

	encoded, reason := runtime.CheckToolList("github", tools)
	require.Empty(t, reason)

	return encoded
}

// reformatted re-spaces a JSON document, as PostgreSQL's jsonb does when it
// returns the stored tools.
func reformatted(t *testing.T, document []byte) []byte {
	t.Helper()

	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, document, "", "  "))

	return indented.Bytes()
}

// newMonitorFixture runs a monitor with a 1 ms interval for a ready
// instance whose stored tools are the upstream's.
func newMonitorFixture(t *testing.T, pingResult func(int) error) *monitorFixture {
	t.Helper()

	template := testTemplate()
	fixture := &monitorFixture{
		upstream: &upstream{pingResult: pingResult, tools: []*mcp.Tool{searchTool}},
		done:     make(chan struct{}),
	}
	fixture.instance = db.Instance{
		ID: instanceID, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: templateUID,
		Desired: "running", Status: "ready", Namespace: instanceNamespace, Tools: reformatted(t, encodeTools(t, searchTool)),
	}
	fixture.store = &healthStore{row: fixture.instance}
	fixture.monitor = &runtime.Monitor{
		Store: fixture.store,
		Template: func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			if key != (types.NamespacedName{Namespace: template.Namespace, Name: template.Name}) {
				return nil, false
			}

			return template.DeepCopy(), true
		},
		Connect:  fixture.upstream.connect,
		Interval: time.Millisecond,
	}

	ctx, cancel := context.WithCancel(t.Context())
	fixture.cancel = cancel

	go func() {
		defer close(fixture.done)

		_ = fixture.monitor.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-fixture.done
	})

	// Run must have started before Step starts monitors.
	require.Eventually(t, func() bool {
		require.NoError(t, fixture.monitor.Step(t.Context(), fixture.instance))
		_, _, connects := fixture.upstream.state()

		return connects > 0
	}, 10*time.Second, time.Millisecond)

	return fixture
}

func TestMonitorHealthCheckSettings(t *testing.T) {
	assert.Equal(t, 30*time.Second, runtime.DefaultHealthInterval)
	assert.Equal(t, 3, runtime.HealthFailureThreshold)
}

func TestMonitorFailsAfterThreeFailedHealthChecksInARow(t *testing.T) {
	fixture := newMonitorFixture(t, func(int) error { return errors.New("connection refused") })

	require.Eventually(t, func() bool {
		row, _ := fixture.store.snapshot()

		return row.Status == "failed"
	}, 10*time.Second, time.Millisecond)

	row, _ := fixture.store.snapshot()
	assert.Equal(t, "stopped", row.Desired)
	assert.Equal(t, "The server didn't answer 3 health checks in a row: connection refused", row.Reason)

	require.Eventually(t, func() bool {
		_, open, _ := fixture.upstream.state()

		return !open
	}, 10*time.Second, time.Millisecond, "the monitoring session is closed")

	pings, _, _ := fixture.upstream.state()
	assert.Equal(t, 3, pings, "the third failure in a row marks it failed")
}

func TestMonitorFailuresMustBeInARow(t *testing.T) {
	// Two failures, then a success, over and over: never three in a row.
	fixture := newMonitorFixture(t, func(n int) error {
		if n%3 == 0 {
			return nil
		}

		return errors.New("timeout")
	})

	require.Eventually(t, func() bool {
		pings, _, _ := fixture.upstream.state()

		return pings >= 12
	}, 10*time.Second, time.Millisecond)

	row, _ := fixture.store.snapshot()
	assert.Equal(t, "ready", row.Status)

	_, _, connects := fixture.upstream.state()
	assert.Greater(t, connects, 1, "a failed ping opens a new session for the next check")
}

func TestMonitorConnectsToTheInstanceEndpoint(t *testing.T) {
	fixture := newMonitorFixture(t, nil)

	fixture.upstream.mu.Lock()
	defer fixture.upstream.mu.Unlock()

	assert.Equal(t, "http://server."+instanceNamespace+".svc:8080/mcp", fixture.upstream.endpoints[0])
}

func TestMonitorStoresChangedTools(t *testing.T) {
	fixture := newMonitorFixture(t, nil)

	require.Eventually(t, func() bool {
		pings, _, _ := fixture.upstream.state()

		return pings >= 3
	}, 10*time.Second, time.Millisecond)

	_, writes := fixture.store.snapshot()
	assert.Zero(t, writes, "an unchanged list isn't written")

	fetch := &mcp.Tool{Name: "fetch", InputSchema: map[string]any{"type": "object"}}
	fixture.upstream.announce([]*mcp.Tool{searchTool, fetch})

	require.Eventually(t, func() bool {
		_, writes := fixture.store.snapshot()

		return writes == 1
	}, 10*time.Second, time.Millisecond)

	row, _ := fixture.store.snapshot()
	assert.Equal(t, "ready", row.Status)

	var stored []map[string]any
	require.NoError(t, json.Unmarshal(row.Tools, &stored))
	require.Len(t, stored, 2)
	assert.Equal(t, "fetch", stored[1]["name"])
}

func TestMonitorFailsWhenChangedToolsFailTheChecks(t *testing.T) {
	fixture := newMonitorFixture(t, nil)

	fixture.upstream.announce([]*mcp.Tool{{Name: strings.Repeat("x", 57)}})

	require.Eventually(t, func() bool {
		row, _ := fixture.store.snapshot()

		return row.Status == "failed"
	}, 10*time.Second, time.Millisecond)

	row, writes := fixture.store.snapshot()
	assert.Equal(t, "Tool `"+strings.Repeat("x", 57)+"` is longer than 64 characters with its `github__` prefix.", row.Reason)
	assert.Zero(t, writes)
}

func TestMonitorStopsWhenInstanceIsNoLongerReady(t *testing.T) {
	fixture := newMonitorFixture(t, nil)

	fixture.store.setStatus("stopping")

	require.Eventually(t, func() bool {
		_, open, _ := fixture.upstream.state()

		return !open
	}, 10*time.Second, time.Millisecond)

	// A new pass doesn't start monitoring a row that isn't ready.
	stopping := fixture.instance
	stopping.Status = "stopping"
	require.NoError(t, fixture.monitor.Step(t.Context(), stopping))

	_, _, connects := fixture.upstream.state()
	assert.Equal(t, 1, connects)
}

func TestMonitorStopsWhenItLosesTheLease(t *testing.T) {
	fixture := newMonitorFixture(t, nil)

	fixture.cancel()
	<-fixture.done

	_, open, _ := fixture.upstream.state()
	assert.False(t, open, "Run returns only after every monitoring session is closed")

	require.NoError(t, fixture.monitor.Step(t.Context(), fixture.instance))

	_, _, connects := fixture.upstream.state()
	assert.Equal(t, 1, connects, "no monitor starts after Run ended")
}

func TestMonitorOneSessionPerInstance(t *testing.T) {
	fixture := newMonitorFixture(t, nil)

	for range 5 {
		require.NoError(t, fixture.monitor.Step(t.Context(), fixture.instance))
	}

	_, _, connects := fixture.upstream.state()
	assert.Equal(t, 1, connects)
}

func TestStarterReadyKstatusFailure(t *testing.T) {
	failedDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instanceNamespace, Labels: instanceLabels(), Generation: 1},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason: "ProgressDeadlineExceeded", Message: "deadline exceeded",
			}},
		},
	}

	t.Run("a failed object marks the instance unhealthy", func(t *testing.T) {
		fixture := newStarterFixture(t, testTemplate(), "ready", readyPod(), failedDeployment)

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, _, _, _ := fixture.store.snapshot()
		assert.Equal(t, "ready", status)

		unhealthy, reason := fixture.store.health()
		assert.True(t, unhealthy)
		assert.Contains(t, reason, "Deployment server failed")
	})

	t.Run("an object in progress leaves it ready", func(t *testing.T) {
		pod := readyPod()
		pod.Status.Conditions[0].Status = corev1.ConditionFalse
		fixture := newStarterFixture(t, testTemplate(), "ready", pod)

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, _, _, writes := fixture.store.snapshot()
		assert.Equal(t, "ready", status)
		assert.Zero(t, writes)
		assert.Empty(t, fixture.listed)
	})
}
