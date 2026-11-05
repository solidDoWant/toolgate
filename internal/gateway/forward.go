package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/version"
)

// MaxResultBytes is the largest tool result the gateway forwards, measured
// as the JSON encoding of the result (docs/design/phase-1.md §7).
const MaxResultBytes = 16 << 20

// lastUsedInterval is how often a call moves an instance's last_used_at
// forward.
const lastUsedInterval = time.Minute

// Reasons a call is refused before policy (docs/design/phase-1.md §3).
const (
	reasonNotEnabled = "not-enabled"
	reasonNotReady   = "not-ready"
)

const tracerName = "github.com/solidDoWant/toolgate/internal/gateway"

// tracer returns the tracer of the request's span, so the gateway's spans
// join the request's trace.
func tracer(ctx context.Context) trace.Tracer {
	return trace.SpanFromContext(ctx).TracerProvider().Tracer(tracerName)
}

// UpstreamEndpoint returns the MCP URL of an instance started from template.
type UpstreamEndpoint func(instance db.Instance, template *v1alpha1.MCPServerTemplate) string

// upstreams keeps this replica's forwarding sessions, one per instance,
// each created on first use.
type upstreams struct {
	endpoint   UpstreamEndpoint
	httpClient *http.Client

	mu       sync.Mutex
	sessions map[string]*upstreamSession
}

type upstreamSession struct {
	mu      sync.Mutex
	session *mcp.ClientSession
}

func newUpstreams(endpoint UpstreamEndpoint, httpClient *http.Client) *upstreams {
	return &upstreams{endpoint: endpoint, httpClient: httpClient, sessions: make(map[string]*upstreamSession)}
}

// session returns the forwarding session to instance, connecting it when
// there is none.
func (u *upstreams) session(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate) (*mcp.ClientSession, error) {
	u.mu.Lock()

	entry, found := u.sessions[instance.ID]
	if !found {
		entry = &upstreamSession{}
		u.sessions[instance.ID] = entry
	}

	u.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.session != nil {
		return entry.session, nil
	}

	// Replicas ignore list_changed on forwarding sessions: the reconciler
	// keeps the tool list.
	client := mcp.NewClient(&mcp.Implementation{Name: "toolgate", Version: version.Get()}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: u.endpoint(instance, template), HTTPClient: u.httpClient}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}

	entry.session = session

	return session, nil
}

// drop closes the forwarding session to an instance, so the next call
// connects again.
func (u *upstreams) drop(id string, session *mcp.ClientSession) {
	u.mu.Lock()
	entry, found := u.sessions[id]
	u.mu.Unlock()

	if !found {
		return
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.session == session {
		entry.session = nil

		_ = session.Close()
	}
}

// dropUnlessReady closes this replica's forwarding session to instance id
// when the instance is no longer ready.
func (m *metaTools) dropUnlessReady(id string) {
	m.upstreams.mu.Lock()
	entry, found := m.upstreams.sessions[id]
	m.upstreams.mu.Unlock()

	if !found {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), fanOutTimeout)
	defer cancel()

	instance, err := db.New(m.database).GetInstance(ctx, id)
	if err == nil && instance.Status == statusReady {
		return
	}

	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return
	}

	m.upstreams.mu.Lock()
	delete(m.upstreams.sessions, id)
	m.upstreams.mu.Unlock()

	entry.mu.Lock()
	session := entry.session
	entry.session = nil
	entry.mu.Unlock()

	// Close waits for the calls in flight on the session. They finish with
	// whatever the upstream answers (row 12), and fan-out must not wait for
	// them.
	if session != nil {
		go func() { _ = session.Close() }()
	}
}

// callTool answers a call of a proxied tool `<server>__<tool>`: it runs the
// Call steps of docs/design/phase-1.md §3. Every refusal writes
// tool.decision, and nothing is forwarded before the allow is committed.
func (m *metaTools) callTool(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	current, ok := requestContextOf(request)
	if !ok {
		return nil, errNoIdentity
	}

	server, tool, _ := strings.Cut(request.Params.Name, toolSeparator)

	// Step 1: the server must be enabled in this session.
	template, enabled, err := m.enabledTemplate(ctx, current, server)
	if err != nil {
		return nil, err
	}

	if !enabled {
		return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonNotEnabled},
			fmt.Sprintf("Server `%s` isn't enabled in this session. Call `%s` first.", server, ToolEnableServer))
	}

	// Step 2: its live instance must be ready.
	instance, status, reason, err := m.readyInstance(ctx, current, template)
	if err != nil {
		return nil, err
	}

	if status != statusReady {
		message := fmt.Sprintf("Server `%s` isn't ready: it is %s.", server, status)
		if reason != "" {
			message = fmt.Sprintf("Server `%s` isn't ready: it is %s: %s", server, status, reason)
		}

		return m.refuseCall(ctx, current, server, tool,
			map[string]any{"reason": reasonNotReady, "status": status, "detail": reason}, message)
	}

	// Step 3: policy.
	_, policySpan := tracer(ctx).Start(ctx, "toolgate.policy")
	decision := m.policy.Decide(policy.Request{Identity: current.identity, Template: template, Tool: tool})
	policySpan.SetAttributes(attribute.Bool("toolgate.allowed", decision.Allowed))
	policySpan.End()

	if !decision.Allowed {
		return m.refuseCall(ctx, current, server, tool, refusalDetails(decision), decision.Reason)
	}

	// Step 4: the allow is committed before anything is forwarded.
	recorder := audit.NewRecorder(m.auditOutput, db.New(m.database))
	if err := recorder.Record(ctx, callEvent(current, "tool.decision", server, tool,
		map[string]any{"decision": "allow", "instance": instance.ID})); err != nil {
		return nil, fmt.Errorf("The call wasn't forwarded: the gateway can't record it. Try again later. (%w)", err) //nolint:staticcheck // A sentence for the model.
	}

	// Step 5.
	started := time.Now()
	result, outcome := m.forward(ctx, instance, template, tool, request.Params.Arguments)

	// Step 6.
	if err := recorder.Record(ctx, callEvent(current, "tool.result", server, tool, map[string]any{
		"outcome": outcome, "instance": instance.ID, "duration_ms": time.Since(started).Milliseconds(),
	})); err != nil {
		return nil, err
	}

	if !instance.LastUsedAt.Valid || time.Since(instance.LastUsedAt.Time) >= lastUsedInterval {
		if err := db.New(m.database).TouchInstance(ctx, instance.ID); err != nil {
			return nil, fmt.Errorf("update instance last_used_at: %w", err)
		}
	}

	return result, nil
}

// forward calls tool on the instance's upstream session. outcome is "ok",
// "error" (the tool reported an error), "too-large" or "failed" (the
// upstream call itself failed).
func (m *metaTools) forward(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate, tool string, arguments json.RawMessage) (*mcp.CallToolResult, string) {
	ctx, span := tracer(ctx).Start(ctx, "toolgate.forward", trace.WithAttributes(
		attribute.String("toolgate.server", template.Name), attribute.String("toolgate.tool", tool)))
	defer span.End()

	session, err := m.upstreams.session(ctx, instance, template)
	if err != nil {
		span.SetStatus(codes.Error, "connect")

		return errorResult(fmt.Sprintf("Server `%s` can't be reached. Try again, or call `%s` to see its status.", template.Name, ToolServerStatus)), "failed"
	}

	params := &mcp.CallToolParams{Name: tool}
	if len(arguments) > 0 {
		params.Arguments = arguments
	}

	result, err := session.CallTool(ctx, params)
	if err != nil {
		span.SetStatus(codes.Error, "call")

		// After a JSON-RPC error response the session still works; after
		// anything else, connect again on the next call.
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) {
			m.upstreams.drop(instance.ID, session)
		}

		return errorResult(fmt.Sprintf("Server `%s` returned an error: %v", template.Name, err)), "failed"
	}

	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > MaxResultBytes {
		span.SetStatus(codes.Error, "too large")

		return errorResult(fmt.Sprintf("The result of `%s` is larger than 16 MiB, the limit, so the gateway didn't forward it.", ProxiedToolName(template.Name, tool))), "too-large"
	}

	if result.IsError {
		return result, "error"
	}

	return result, "ok"
}

// enabledTemplate returns the template of the session's enabled server
// called server.
func (m *metaTools) enabledTemplate(ctx context.Context, current requestContext, server string) (*v1alpha1.MCPServerTemplate, bool, error) {
	rows, err := db.New(m.database).ListSessionServers(ctx, current.session.ID)
	if err != nil {
		return nil, false, fmt.Errorf("read enabled servers: %w", err)
	}

	for _, row := range rows {
		if row.TemplateName != server {
			continue
		}

		key := types.NamespacedName{Namespace: row.TemplateNs, Name: row.TemplateName}
		if template, found := m.catalog.Running(key); found {
			return template, true, nil
		}

		// The template is gone: the server stays enabled with no instance.
		template := &v1alpha1.MCPServerTemplate{}
		template.Namespace, template.Name = key.Namespace, key.Name

		return template, true, nil
	}

	return nil, false, nil
}

// readyInstance returns the live instance for template under its current
// scope. When it isn't ready, status and reason are what clients are shown
// for the newest instance (a failed one keeps its failure reason).
func (m *metaTools) readyInstance(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate) (db.Instance, string, string, error) {
	if template.UID == "" {
		return db.Instance{}, StatusStopped, "the server was removed from the catalog", nil
	}

	instance, err := db.New(m.database).GetLiveInstance(ctx, db.GetLiveInstanceParams{
		TemplateNs: template.Namespace, TemplateName: template.Name,
		ScopeKey: scopeKey(template.Spec.Scope, current.identity),
	})

	switch {
	case err == nil && instance.Status == statusReady:
		return instance, statusReady, "", nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return db.Instance{}, "", "", fmt.Errorf("read live instance: %w", err)
	}

	newest, found, err := m.newestInstance(ctx, current.identity, template)
	if err != nil {
		return db.Instance{}, "", "", err
	}

	if !found {
		return db.Instance{}, StatusStopped, "", nil
	}

	status, reason := shownStatus(newest)
	if status == StatusReady {
		// The newest row is ready but not live: it is being stopped.
		status = StatusStopped
	}

	return db.Instance{}, status, reason, nil
}

// refuseCall writes the refusing tool.decision and returns message to the
// model as a tool error.
func (m *metaTools) refuseCall(ctx context.Context, current requestContext, server, tool string, details map[string]any, message string) (*mcp.CallToolResult, error) {
	details["decision"] = "refuse"

	recorder := audit.NewRecorder(m.auditOutput, db.New(m.database))
	if err := recorder.Record(ctx, callEvent(current, "tool.decision", server, tool, details)); err != nil {
		return nil, err
	}

	return errorResult(message), nil
}

func callEvent(current requestContext, eventType, server, tool string, details map[string]any) audit.Event {
	event := auditEvent(current, eventType, server, details)
	event.Tool = tool

	return event
}

func errorResult(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}
}
