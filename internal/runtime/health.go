package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/upstreamlimit"
)

// Health check settings (docs/design/phase-1.md §7).
const (
	// DefaultHealthInterval is how often the reconciler pings each ready
	// instance.
	DefaultHealthInterval = 30 * time.Second
	// HealthFailureThreshold is the number of health checks in a row that
	// must fail before a ready instance is marked failed.
	HealthFailureThreshold = 3
)

// StatusReady is the status of an instance that answers MCP.
const StatusReady = "ready"

// HealthStore is what the health monitor reads and writes in PostgreSQL.
// Its writes are compare-and-sets on the row being ready.
type HealthStore interface {
	// Instance returns the current instance row.
	Instance(ctx context.Context, id string) (db.Instance, error)
	// MarkFailed moves an instance from status from to failed.
	MarkFailed(ctx context.Context, id, from, reason string) (bool, error)
	// SetTools replaces the tool list of a ready instance.
	SetTools(ctx context.Context, id string, tools []byte) (bool, error)
}

// MonitorSession is a monitoring MCP session to one instance.
type MonitorSession interface {
	Ping(ctx context.Context) error
	ListTools(ctx context.Context) ([]*mcp.Tool, error)
	Close() error
}

// MonitorConnector opens a monitoring session to endpoint. It calls
// toolsChanged each time the upstream announces a changed tool list.
type MonitorConnector func(ctx context.Context, endpoint string, toolsChanged func()) (MonitorSession, error)

// Monitor keeps one monitoring MCP session to each ready instance. It pings
// the instance every Interval and marks it failed after
// HealthFailureThreshold failures in a row. When the upstream announces
// `list_changed`, it re-lists the tools, checks them, and stores them.
//
// Run must be running (it runs only on the Lease holder) for Step to start
// monitors. Monitors end when Run ends, so a replica that loses the Lease
// stops monitoring.
type Monitor struct {
	Store HealthStore
	// Template returns the template an instance was started from.
	Template func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool)
	Connect  MonitorConnector
	// Interval between health checks. Zero means DefaultHealthInterval.
	Interval time.Duration
	Logger   *slog.Logger

	mu       sync.Mutex
	base     context.Context
	watchers map[string]struct{}
	running  sync.WaitGroup
}

// Run lets Step start monitors until ctx ends, then stops them all.
func (m *Monitor) Run(ctx context.Context) error {
	m.mu.Lock()
	m.base = ctx
	m.mu.Unlock()

	<-ctx.Done()

	m.mu.Lock()
	m.base = nil
	m.mu.Unlock()

	m.running.Wait()

	return nil
}

// NeedLeaderElection makes the manager run the monitor only on the Lease
// holder.
func (m *Monitor) NeedLeaderElection() bool { return true }

// Step starts a monitor for a ready instance that has none.
func (m *Monitor) Step(_ context.Context, instance db.Instance) error {
	if instance.Desired != "running" || instance.Status != StatusReady {
		return nil
	}

	template, found := m.Template(types.NamespacedName{Namespace: instance.TemplateNs, Name: instance.TemplateName})
	if !found || string(template.UID) != instance.TemplateUid {
		// The stop path handles a deleted or re-created template.
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.base == nil || m.base.Err() != nil {
		return nil
	}

	if _, watching := m.watchers[instance.ID]; watching {
		return nil
	}

	if m.watchers == nil {
		m.watchers = map[string]struct{}{}
	}

	m.watchers[instance.ID] = struct{}{}
	m.running.Add(1)

	go func(ctx context.Context) {
		defer m.running.Done()
		defer m.forget(instance.ID)

		m.watch(ctx, instance, EndpointURL(instance, template))
	}(m.base)

	return nil
}

func (m *Monitor) forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.watchers, id)
}

// watcher is the state of one instance's monitor.
type watcher struct {
	monitor  *Monitor
	instance db.Instance
	endpoint string
	session  MonitorSession
	failures int
	// stored is the tool list last read from or written to the row.
	stored  []byte
	changed chan struct{}
}

// watch monitors one instance until it is no longer ready, it is marked
// failed, or ctx ends. It checks at once and then every Interval.
func (m *Monitor) watch(ctx context.Context, instance db.Instance, endpoint string) {
	state := &watcher{monitor: m, instance: instance, endpoint: endpoint, stored: instance.Tools,
		changed: make(chan struct{}, 1)}

	defer func() {
		if state.session != nil {
			_ = state.session.Close()
		}
	}()

	ticker := time.NewTicker(m.interval())
	defer ticker.Stop()

	if !state.check(ctx) {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-state.changed:
			if !state.refreshTools(ctx) {
				return
			}
		case <-ticker.C:
			if !state.check(ctx) {
				return
			}
		}
	}
}

func (w *watcher) toolsChanged() {
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// check runs one health check. It returns false when monitoring ends: the
// row is no longer ready, or it was marked failed.
func (w *watcher) check(ctx context.Context) bool {
	current, err := w.monitor.Store.Instance(ctx, w.instance.ID)
	if err != nil {
		if ctx.Err() == nil {
			w.monitor.logger().WarnContext(ctx, "health check: can't read instance",
				slog.String("instance", w.instance.ID), slog.String("error", err.Error()))
		}

		return ctx.Err() == nil
	}

	if current.Desired != "running" || current.Status != StatusReady {
		return false
	}

	w.stored = current.Tools

	err = w.ping(ctx)
	if ctx.Err() != nil {
		return false
	}

	if err == nil {
		w.failures = 0

		return true
	}

	w.failures++
	w.monitor.logger().InfoContext(ctx, "health check failed", slog.String("instance", w.instance.ID),
		slog.Int("failures", w.failures), slog.String("error", err.Error()))

	if w.failures < HealthFailureThreshold {
		return true
	}

	w.monitor.fail(ctx, w.instance, "The server didn't answer "+strconv.Itoa(HealthFailureThreshold)+
		" health checks in a row: "+err.Error())

	return false
}

// ping pings the instance, opening the monitoring session first if there is
// none. A new session re-lists the tools, so a change announced while there
// was no session isn't missed. A session whose ping failed is closed, and
// the next check opens a new one.
func (w *watcher) ping(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, w.monitor.interval())
	defer cancel()

	if w.session == nil {
		opened, err := w.monitor.Connect(checkCtx, w.endpoint, w.toolsChanged)
		if err != nil {
			return err
		}

		w.session = opened
		w.toolsChanged()
	}

	if err := w.session.Ping(checkCtx); err != nil {
		_ = w.session.Close()
		w.session = nil

		return err
	}

	return nil
}

// refreshTools re-lists the tools, checks them and stores them when they
// differ from the row. It returns false when monitoring ends: the instance
// is no longer ready, or the list failed the checks.
func (w *watcher) refreshTools(ctx context.Context) bool {
	if w.session == nil {
		return true
	}

	// The re-list is bounded like a health check, so an upstream that
	// never answers `tools/list` can't stop the checks.
	listCtx, cancel := context.WithTimeout(ctx, w.monitor.interval())
	tools, err := w.session.ListTools(listCtx)

	cancel()

	if errors.Is(err, ErrToolListTooLarge) {
		w.monitor.fail(ctx, w.instance, toolListTooLargeReason)

		return false
	}

	if err != nil {
		// A broken session also fails the next health check.
		w.monitor.logger().InfoContext(ctx, "re-listing changed tools failed", slog.String("instance", w.instance.ID),
			slog.String("error", err.Error()))

		return ctx.Err() == nil
	}

	encoded, reason := CheckToolList(w.instance.TemplateName, tools)
	if reason != "" {
		w.monitor.fail(ctx, w.instance, reason)

		return false
	}

	if sameJSON(encoded, w.stored) {
		return true
	}

	stored, err := w.monitor.Store.SetTools(ctx, w.instance.ID, encoded)
	if err != nil {
		w.monitor.logger().WarnContext(ctx, "storing changed tools failed", slog.String("instance", w.instance.ID),
			slog.String("error", err.Error()))

		return ctx.Err() == nil
	}

	if stored {
		w.stored = encoded
	}

	return stored
}

// sameJSON reports whether two JSON documents are equal as parsed values.
// The tools column is jsonb, which doesn't keep key order or spacing.
func sameJSON(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}

	return reflect.DeepEqual(leftValue, rightValue)
}

func (m *Monitor) interval() time.Duration {
	if m.Interval == 0 {
		return DefaultHealthInterval
	}

	return m.Interval
}

func (m *Monitor) fail(ctx context.Context, instance db.Instance, reason string) {
	if _, err := m.Store.MarkFailed(ctx, instance.ID, StatusReady, truncate(reason, maxReasonBytes)); err != nil {
		m.logger().WarnContext(ctx, "marking instance failed", slog.String("instance", instance.ID),
			slog.String("error", err.Error()))
	}
}

func (m *Monitor) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}

	return slog.Default()
}

// MCPMonitorConnector is the MonitorConnector that speaks MCP over
// Streamable HTTP. It keeps the standalone stream open, so the upstream
// can announce `list_changed`.
func MCPMonitorConnector(ctx context.Context, endpoint string, toolsChanged func()) (MonitorSession, error) {
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { toolsChanged() },
	})

	session, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: toolListHTTPClient}, nil)
	if err != nil {
		return nil, fmt.Errorf("initialize %s: %w", endpoint, err)
	}

	return mcpMonitorSession{session: session}, nil
}

type mcpMonitorSession struct {
	session *mcp.ClientSession
}

func (s mcpMonitorSession) Ping(ctx context.Context) error {
	if err := s.session.Ping(ctx, nil); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	return nil
}

func (s mcpMonitorSession) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool

	ctx, budget := upstreamlimit.WithBudget(ctx, toolListResponseBudget)

	for tool, err := range s.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", toolListError(err, budget))
		}

		tools = append(tools, tool)
		if len(tools) > MaxTools {
			break
		}
	}

	return tools, nil
}

func (s mcpMonitorSession) Close() error { return s.session.Close() }
