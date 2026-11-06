package runtime_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// probeTarget is a fake health check target. answer decides the status of
// request number n (counted from 1).
type probeTarget struct {
	mu     sync.Mutex
	urls   []string
	answer func(n int) int
}

func (p *probeTarget) probe(_ context.Context, url string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.urls = append(p.urls, url)

	return p.answer(len(p.urls)), nil
}

func (p *probeTarget) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.urls...)
}

type checkFixture struct {
	store    *healthStore
	upstream *upstream
	target   *probeTarget
}

// newCheckFixture runs a monitor for a ready instance whose health_check
// column is healthCheck (nil for none).
func newCheckFixture(t *testing.T, healthCheck string, answer func(int) int, pingResult func(int) error, configure ...func(*runtime.Monitor, *checkFixture)) *checkFixture {
	t.Helper()

	template := testTemplate()
	fixture := &checkFixture{
		upstream: &upstream{pingResult: pingResult, tools: []*mcp.Tool{searchTool}},
		target:   &probeTarget{answer: answer},
	}

	instance := db.Instance{
		ID: instanceID, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: templateUID,
		Desired: "running", Status: "ready", Namespace: instanceNamespace, Tools: encodeTools(t, searchTool),
	}
	if healthCheck != "" {
		instance.HealthCheck = []byte(healthCheck)
	}

	fixture.store = &healthStore{row: instance}
	monitor := &runtime.Monitor{
		Store: fixture.store,
		Template: func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			if key != (types.NamespacedName{Namespace: template.Namespace, Name: template.Name}) {
				return nil, false
			}

			return template.DeepCopy(), true
		},
		Connect:  fixture.upstream.connect,
		Interval: time.Millisecond,
		Probe:    fixture.target.probe,
	}

	for _, option := range configure {
		option(monitor, fixture)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = monitor.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		require.NoError(t, monitor.Step(t.Context(), instance))

		_, _, connects := fixture.upstream.state()

		return connects > 0
	}, 10*time.Second, time.Millisecond)

	return fixture
}

func (f *checkFixture) writes() []healthWrite {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()

	return append([]healthWrite(nil), f.store.healthWrites...)
}

const fastCheck = `{"http":{"service":"db-proxy","port":8080,"path":"/healthz","expectedStatus":200},` +
	`"interval":"1ms","timeout":"1s","failureThreshold":3,"successThreshold":2,"onFailure":"none","maxRepairs":3}`

func always(status int) func(int) int { return func(int) int { return status } }

func TestMonitorCustomCheckMarksUnhealthyAtFailureThreshold(t *testing.T) {
	fixture := newCheckFixture(t, fastCheck, always(http.StatusServiceUnavailable), nil)

	require.Eventually(t, func() bool { return len(fixture.writes()) > 0 }, 10*time.Second, time.Millisecond)

	writes := fixture.writes()
	assert.Equal(t, []healthWrite{{healthy: false, reason: "GET http://db-proxy:8080/healthz answered 503, expected 200"}}, writes)

	row, _ := fixture.store.snapshot()
	assert.Equal(t, "ready", row.Status, "an unhealthy instance stays ready")
	assert.True(t, row.UnhealthySince.Valid)

	requests := fixture.target.requests()
	require.NotEmpty(t, requests)
	assert.Equal(t, "http://db-proxy."+instanceNamespace+".svc:8080/healthz", requests[0])
}

func TestMonitorCustomCheckCountsFailuresInARow(t *testing.T) {
	// The third failure in a row, and not an earlier one, marks the
	// instance unhealthy.
	twice := newCheckFixture(t, fastCheck, func(n int) int {
		if n <= 2 {
			return http.StatusServiceUnavailable
		}

		return http.StatusOK
	}, nil)

	require.Eventually(t, func() bool { return len(twice.target.requests()) >= 10 }, 10*time.Second, time.Millisecond)
	assert.Empty(t, twice.writes(), "two failures are below the threshold of 3")

	exactly := newCheckFixture(t, fastCheck, func(n int) int {
		if n <= 3 {
			return http.StatusServiceUnavailable
		}

		return http.StatusOK
	}, nil)

	require.Eventually(t, func() bool { return len(exactly.writes()) > 0 }, 10*time.Second, time.Millisecond)
	assert.False(t, exactly.writes()[0].healthy, "three failures reach the threshold of 3")
}

func TestMonitorCustomCheckCounterResetsOnPass(t *testing.T) {
	// Two failures, then a pass, over and over: never three in a row.
	fixture := newCheckFixture(t, fastCheck, func(n int) int {
		if n%3 == 0 {
			return http.StatusOK
		}

		return http.StatusServiceUnavailable
	}, nil)

	require.Eventually(t, func() bool { return len(fixture.target.requests()) >= 30 }, 10*time.Second, time.Millisecond)
	assert.Empty(t, fixture.writes())
}

func TestMonitorCustomCheckMarksHealthyAtSuccessThreshold(t *testing.T) {
	// Three failures, then one pass, a failure, and passes from then on.
	// The lone pass is below the success threshold of 2, so the instance
	// is healthy again only after passes 6 and 7.
	var (
		store        *healthStore
		earlyHealthy atomic.Bool
	)

	answer := func(n int) int {
		if n == 7 {
			store.mu.Lock()
			earlyHealthy.Store(len(store.healthWrites) > 1)
			store.mu.Unlock()
		}

		switch {
		case n <= 3, n == 5:
			return http.StatusServiceUnavailable
		default:
			return http.StatusOK
		}
	}

	fixture := newCheckFixture(t, fastCheck, answer, nil, func(_ *runtime.Monitor, fixture *checkFixture) {
		store = fixture.store
	})

	require.Eventually(t, func() bool { return len(fixture.writes()) >= 2 }, 10*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(fixture.target.requests()) >= 7 }, 10*time.Second, time.Millisecond)

	writes := fixture.writes()
	assert.False(t, writes[0].healthy)
	assert.Equal(t, healthWrite{healthy: true}, writes[1])
	assert.False(t, earlyHealthy.Load(), "healthy again before the second pass in a row")

	row, _ := fixture.store.snapshot()
	assert.False(t, row.UnhealthySince.Valid)
	assert.Empty(t, row.UnhealthyReason)
}

// hangingSession is a monitoring session whose ping answers only when its
// context ends.
type hangingSession struct{ runtime.MonitorSession }

func (hangingSession) Ping(ctx context.Context) error {
	<-ctx.Done()

	return ctx.Err()
}

func TestMonitorCustomCheckRunsWhilePingHangs(t *testing.T) {
	// The ping hangs for its whole timeout, an hour here; the check still
	// reaches its threshold.
	fixture := newCheckFixture(t, fastCheck, always(http.StatusServiceUnavailable), nil, func(monitor *runtime.Monitor, fixture *checkFixture) {
		monitor.Interval = time.Hour
		monitor.Connect = func(ctx context.Context, endpoint string, changed func()) (runtime.MonitorSession, error) {
			session, err := fixture.upstream.connect(ctx, endpoint, changed)

			return hangingSession{MonitorSession: session}, err
		}
	})

	require.Eventually(t, func() bool { return len(fixture.writes()) > 0 }, 10*time.Second, time.Millisecond)
}

func TestMonitorWithoutCustomCheckOnlyPings(t *testing.T) {
	fixture := newCheckFixture(t, "", always(http.StatusServiceUnavailable), nil)

	require.Eventually(t, func() bool {
		pings, _, _ := fixture.upstream.state()

		return pings >= 10
	}, 10*time.Second, time.Millisecond)

	assert.Empty(t, fixture.target.requests(), "no custom check runs")
	assert.Empty(t, fixture.writes())
}

func TestHTTPHealthProbeDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			http.Redirect(writer, request, "/elsewhere", http.StatusFound)

			return
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	status, err := runtime.HTTPHealthProbe(t.Context(), target.URL+"/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusFound, status)
}

func TestHTTPHealthProbeReadsAtMost64KiB(t *testing.T) {
	// The target sends more than 64 KiB and then never ends the body. The
	// probe must return without reading the rest.
	done := make(chan struct{})

	t.Cleanup(func() { close(done) })

	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(strings.Repeat("x", runtime.MaxHealthCheckBody+1)))
		writer.(http.Flusher).Flush()

		select {
		case <-request.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(target.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	status, err := runtime.HTTPHealthProbe(ctx, target.URL+"/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.NoError(t, ctx.Err(), "the probe returned before its context ended")
	assert.Equal(t, 64<<10, runtime.MaxHealthCheckBody)
}

func TestHTTPHealthProbeEndsAtItsContext(t *testing.T) {
	done := make(chan struct{})

	t.Cleanup(func() { close(done) })

	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(target.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err := runtime.HTTPHealthProbe(ctx, target.URL+"/healthz")
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}
