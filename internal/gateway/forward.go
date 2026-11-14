package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
	"github.com/solidDoWant/toolgate/internal/upstreamlimit"
)

// MaxResultBytes is the largest tool result the gateway forwards, measured
// as the JSON encoding of the result.
const MaxResultBytes = 16 << 20

// upstreamResponseBudget bounds the upstream response bytes one forwarded
// call reads, so a huge result is refused without being read whole.
const upstreamResponseBudget = MaxResultBytes + upstreamlimit.Slack

// lastUsedInterval is how often a call moves an instance's last_used_at
// forward.
const lastUsedInterval = time.Minute

// Reasons a call is refused before policy.
const (
	reasonNotEnabled = "not-enabled"
	reasonNotReady   = "not-ready"
	reasonNotOffered = "not-offered"
)

// reasonPayloadError is the refusal reason of a call whose arguments
// couldn't be stored as an audit payload.
const reasonPayloadError = "payload-error"

const tracerName = "github.com/solidDoWant/toolgate/internal/gateway"

// tracer returns the tracer of the request's span, so the gateway's spans
// join the request's trace.
func tracer(ctx context.Context) trace.Tracer {
	return trace.SpanFromContext(ctx).TracerProvider().Tracer(tracerName)
}

// UpstreamEndpoint returns the MCP URL of an instance started from template.
type UpstreamEndpoint func(instance db.Instance, template *v1alpha1.MCPServerTemplate) string

// upstreamConnectContext returns a context for connecting to an upstream
// that keeps only ctx's cancellation, deadline and trace span. The go-sdk
// keeps the protocol revision of the request being served in its context,
// and its client would send that revision on the upstream connection, which
// negotiates its own.
func upstreamConnectContext(ctx context.Context) (context.Context, context.CancelFunc) {
	detached := trace.ContextWithSpan(context.Background(), trace.SpanFromContext(ctx))

	var cancelDeadline context.CancelFunc = func() {}
	if deadline, ok := ctx.Deadline(); ok {
		detached, cancelDeadline = context.WithDeadline(detached, deadline)
	}

	detached, cancel := context.WithCancel(detached)
	stop := context.AfterFunc(ctx, cancel)

	return detached, func() {
		stop()
		cancel()
		cancelDeadline()
	}
}

// callTool answers a call of a proxied tool `<server>__<tool>`: it runs the
// call steps. Every refusal writes
// tool.decision, and nothing is forwarded before the allow is committed.
func (m *metaTools) callTool(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	observation := &callObservation{started: time.Now(), server: unknownServer}
	ctx = context.WithValue(ctx, callObservationKey{}, observation)

	result, err := m.proxyCall(ctx, request)
	m.observeCall(observation, err)

	return result, err
}

func (m *metaTools) proxyCall(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	current, ok := requestContextOf(request)
	if !ok {
		return nil, errNoIdentity
	}

	server, exposed, _ := splitProxiedName(request.Params.Name)
	tool := exposed

	// Step 1: find the server and the tool. A server that isn't enabled in
	// this session is still found, since a call of an allowed server enables
	// it.
	template, enabled, err := m.enabledTemplate(ctx, current, server)
	if err != nil {
		return nil, err
	}

	if enabled {
		observationOf(ctx).server = template.Name
	}

	if !enabled {
		offered, offerErr := m.catalog.Offered(server)
		if offerErr != nil {
			return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonNotOffered},
				fmt.Sprintf("Server `%s` has no tool `%s`.", server, exposed))
		}

		template = offered
		observationOf(ctx).server = template.Name
	}

	// The exposed name maps to the upstream name from the template alone. A
	// hidden tool, or a renamed tool called by its upstream name, is not
	// offered. The call carries the upstream name from here on.
	upstream, offered := toolcatalog.Upstream(template, exposed)
	tool = upstream

	if exposedName := toolcatalog.ExposedName(template, upstream); exposedName != upstream {
		ctx = withExposedTool(ctx, exposedName)
	}

	if !offered {
		return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonNotOffered},
			fmt.Sprintf("Server `%s` has no tool `%s`.", server, exposed))
	}

	// A call of a tool the session's latest search returned is a hit,
	// recorded before anything else is decided.
	if err := m.recordSearchHit(ctx, current, template, tool); err != nil {
		return nil, err
	}

	// Steps 2 and 3: a server that isn't enabled yet is enabled by the call
	// when policy allows it.
	if !enabled {
		result, err := m.enableForCall(ctx, request, current, template, tool)
		if result != nil || err != nil {
			return result, err
		}
	}

	// The built-in server has no instance: its calls skip the instance
	// steps and go on to policy.
	if isBuiltin(template) {
		return m.decideAndRun(ctx, current, request, template, db.Instance{}, tool)
	}

	// Step 2: its live instance must be ready. A call to an instance that is
	// still starting, such as a restored or replaced one, waits for it as an
	// enable does.
	instance, status, reason, err := m.readyInstance(ctx, current, template)
	if err != nil {
		return nil, err
	}

	// Step 5: an `onCall` server with no live instance starts on this call,
	// unless its newest instance failed on the current generation.
	if instance.ID == "" && template.UID != "" {
		// The call is decided with fresh state before an instance is
		// inserted: a refused principal must not start the server. The
		// decision that follows the start is still the one that is
		// recorded for an allow.
		state, err := m.policyState(ctx, current.identity)
		if err != nil {
			return nil, err
		}

		if decision := m.decide(ctx, m.policyRequest(current, template, tool, state)); !decision.Allowed {
			m.metrics.ObservePolicyRefusal(decision.Step, metricReason(decision))

			return m.refuseCall(ctx, current, server, tool, refusalDetails(decision), decision.Reason)
		}

		started, refusal, err := m.startForCall(ctx, current, template)
		if err != nil {
			return nil, err
		}

		if refusal != nil {
			observationOf(ctx).setOutcome(runtime.CallStartFailed)

			return m.refuseCall(ctx, current, server, tool, refusal.details, refusal.message)
		}

		if started.ID != "" {
			instance, status, reason = started, StatusStarting, ""
			if started.Status == statusReady {
				status = statusReady
			}
		}
	}

	if status == StatusStarting && instance.ID != "" {
		m.metrics.WaitingForStart(1)

		instance, status, reason, err = m.waitForCall(ctx, request, current, template, instance)

		m.metrics.WaitingForStart(-1)

		if err != nil {
			var timeout startTimeoutError
			if errors.As(err, &timeout) {
				observationOf(ctx).setOutcome(runtime.CallTimeout)
			}

			return m.refuseCall(ctx, current, server, tool,
				map[string]any{"reason": reasonNotReady, "status": StatusStarting, "detail": err.Error()}, err.Error())
		}
	}

	if status != statusReady {
		if status == statusFailed {
			observationOf(ctx).setOutcome(runtime.CallStartFailed)
		}

		message := fmt.Sprintf("Server `%s` isn't ready: it is %s.", server, status)
		if reason != "" {
			message = fmt.Sprintf("Server `%s` isn't ready: it is %s: %s", server, status, reason)
		}

		return m.refuseCall(ctx, current, server, tool,
			map[string]any{"reason": reasonNotReady, "status": status, "detail": reason}, message)
	}

	if instance.UnhealthySince.Valid {
		return m.refuseCall(ctx, current, server, tool,
			map[string]any{"reason": reasonNotReady, "status": status, "detail": instance.UnhealthyReason},
			unhealthyMessage(server, instance))
	}

	// Step 6: the ready instance must still offer the tool.
	listed, err := instanceOffers(instance, template, tool)
	if err != nil {
		return nil, err
	}

	if !listed {
		return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonNotOffered},
			fmt.Sprintf("Server `%s` doesn't offer `%s` any more.", server, exposed))
	}

	return m.decideAndRun(ctx, current, request, template, instance, tool)
}

// decideAndRun is the policy step of a call and what follows it: a refusal,
// an approval, or the committed allow, the forward and tool.result.
func (m *metaTools) decideAndRun(ctx context.Context, current requestContext, request *mcp.CallToolRequest,
	template *v1alpha1.MCPServerTemplate, instance db.Instance, tool string,
) (*mcp.CallToolResult, error) {
	server := template.Name

	// Step 3: policy.
	state, err := m.policyState(ctx, current.identity)
	if err != nil {
		return nil, err
	}

	decision := m.decide(ctx, m.policyRequest(current, template, tool, state))

	if !decision.Allowed {
		m.metrics.ObservePolicyRefusal(decision.Step, metricReason(decision))

		return m.refuseCall(ctx, current, server, tool, refusalDetails(decision), decision.Reason)
	}

	if decision.NeedsApproval {
		return m.callWithApproval(ctx, current, request, template, tool)
	}

	// Step 4: the allow is committed before anything is forwarded. With
	// payloads on, the arguments are stored first; a call whose arguments
	// can't be stored is refused.
	allowDetails := map[string]any{"decision": "allow"}
	if instance.ID != "" {
		allowDetails["instance"] = instance.ID
	}

	if decision.Grant != "" {
		allowDetails["grant"] = decision.Grant
	}

	allow := callEvent(current, "tool.decision", server, tool, withExposedDetail(ctx, allowDetails))
	if err := m.payloads.Attach(ctx, &allow, callArguments(request.Params.Arguments)); err != nil {
		return m.refuseCall(ctx, current, server, tool,
			map[string]any{"reason": reasonPayloadError, "payload_error": err.Error()}, messageNotRecorded)
	}

	recorder := audit.NewRecorder(m.auditOutput, db.New(m.database))
	if err := recorder.Record(ctx, allow); err != nil {
		return nil, fmt.Errorf("The call wasn't forwarded: the gateway can't record it. Try again later. (%w)", err) //nolint:staticcheck // A sentence for the model.
	}

	return m.forwardAndRecord(ctx, current, request, instance, template, tool, request.Params.Arguments)
}

// forwardAndRecord is the last part of a call: it forwards an allowed
// call to instance and writes tool.result.
func (m *metaTools) forwardAndRecord(ctx context.Context, current requestContext, request *mcp.CallToolRequest, instance db.Instance, template *v1alpha1.MCPServerTemplate, tool string, arguments json.RawMessage) (*mcp.CallToolResult, error) {
	server := template.Name
	recorder := audit.NewRecorder(m.auditOutput, db.New(m.database))

	// Step 5. A built-in tool runs here instead of being forwarded.
	started := time.Now()

	var (
		result  *mcp.CallToolResult
		outcome string
	)

	if isBuiltin(template) {
		var err error

		result, outcome, err = m.callBuiltin(ctx, current, request, tool, arguments)
		if err != nil {
			return nil, err
		}
	} else {
		stopBeat := m.keepInstanceBusy(ctx, instance)
		result, outcome = m.forward(ctx, current, instance, template, tool, arguments)

		stopBeat()
		observationOf(ctx).forwarded(outcome)
	}

	// Step 6. A result that can't be stored is still returned; the row
	// records why its payload is missing.
	resultDetails := map[string]any{"outcome": outcome, "duration_ms": time.Since(started).Milliseconds()}
	if instance.ID != "" {
		resultDetails["instance"] = instance.ID
	}

	resultEvent := callEvent(current, "tool.result", server, tool, withExposedDetail(ctx, resultDetails))
	if err := m.payloads.Attach(ctx, &resultEvent, result); err != nil {
		resultEvent.Details["payload_error"] = err.Error()
	}

	if err := recorder.Record(ctx, resultEvent); err != nil {
		return nil, err
	}

	if instance.ID != "" && (!instance.LastUsedAt.Valid || time.Since(instance.LastUsedAt.Time) >= lastUsedInterval) {
		if err := db.New(m.database).TouchInstance(ctx, instance.ID); err != nil {
			return nil, fmt.Errorf("update instance last_used_at: %w", err)
		}
	}

	return result, nil
}

// keepInstanceBusy moves the instance's last_used_at to now and then every
// lastUsedInterval until the returned function is called, so the update
// policy never sees an instance with a call in flight as idle. A failed
// write is not fatal: the call's end writes it again.
func (m *metaTools) keepInstanceBusy(ctx context.Context, instance db.Instance) func() {
	if instance.ID == "" {
		return func() {}
	}

	queries := db.New(m.database)
	beatCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(testtime.Scale(lastUsedInterval))
		defer ticker.Stop()

		for {
			_ = queries.TouchInstance(beatCtx, instance.ID)

			select {
			case <-beatCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// decide asks the policy engine for a decision inside a toolgate.policy
// span.
func (m *metaTools) decide(ctx context.Context, request policy.Request) policy.Decision {
	_, span := tracer(ctx).Start(ctx, "toolgate.policy")
	decision := m.policy.Decide(request)
	span.SetAttributes(attribute.Bool("toolgate.allowed", decision.Allowed))
	span.End()

	return decision
}

// forward calls tool on the instance's upstream session. outcome is "ok",
// "error" (the tool reported an error), "too-large" or "failed" (the
// upstream call itself failed).
func (m *metaTools) forward(ctx context.Context, current requestContext, instance db.Instance, template *v1alpha1.MCPServerTemplate, tool string, arguments json.RawMessage) (*mcp.CallToolResult, string) {
	ctx, span := tracer(ctx).Start(ctx, "toolgate.forward", trace.WithAttributes(
		attribute.String("toolgate.server", template.Name), attribute.String("toolgate.tool", tool)))
	defer span.End()

	ctx, budget := upstreamlimit.WithBudget(ctx, upstreamResponseBudget)

	upstream, err := m.upstreams.acquire(ctx, m.database, instance, template, remoteIdentity(current), current.session.ID)
	if err != nil {
		span.SetStatus(codes.Error, "connect")

		var missing *runtime.MissingSecretError

		switch {
		case errors.As(err, &missing):
			return errorResult(missing.Error()), "failed"
		case errors.Is(err, runtime.ErrCredentialRefused):
			return errorResult(runtime.CredentialRefusedReason(template)), "failed"
		}

		return errorResult(fmt.Sprintf("Server `%s` can't be reached. Try again, or call `%s` to see its status.", template.Name, ToolServerStatus)), "failed"
	}

	params := &mcp.CallToolParams{Name: tool}
	if len(arguments) > 0 {
		params.Arguments = arguments
	}

	tooLarge := func() (*mcp.CallToolResult, string) {
		span.SetStatus(codes.Error, "too large")

		return errorResult(fmt.Sprintf("The result of `%s` is larger than 16 MiB, the limit, so the gateway didn't forward it.", ProxiedToolName(template.Name, tool))), "too-large"
	}

	// A copy that belongs to no stored row serves this one call, and then
	// its upstream session ends.
	if upstream.once {
		defer upstream.End()
	}

	result, err := upstream.session.CallTool(ctx, params)
	if err != nil {
		span.SetStatus(codes.Error, "call")

		// After a JSON-RPC error response the copy still works; after
		// anything else, the next call makes a new copy. The upstream
		// session and its row stay.
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) && !upstream.once {
			m.upstreams.release(upstream.key, upstream.upstreamCopy)
		}

		// A refused credential stays refused until the user sets the
		// secret again, which the next copy reads.
		if upstream.remote != nil && upstream.remote.Refused() {
			if !upstream.once {
				m.upstreams.release(upstream.key, upstream.upstreamCopy)
			}

			return errorResult(runtime.CredentialRefusedReason(template)), "failed"
		}

		// The response was cut off at the budget.
		if budget.Exceeded() {
			return tooLarge()
		}

		return errorResult(fmt.Sprintf("Server `%s` returned an error: %v", template.Name, err)), "failed"
	}

	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > MaxResultBytes {
		return tooLarge()
	}

	if result.IsError {
		return result, "error"
	}

	return result, "ok"
}

// remoteIdentity is the identity a remote server's headers are filled in
// for: the caller's verified identity, never a claimed field beyond what
// the header variables may name.
func remoteIdentity(current requestContext) runtime.Identity {
	return runtime.Identity{
		PrincipalID:     current.identity.Principal,
		PrincipalEmail:  current.identity.Email,
		PrincipalName:   current.session.Name,
		PrincipalGroups: current.identity.Groups,
		Kind:            current.identity.Kind,
	}
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
	case err == nil && (instance.Status == "pending" || instance.Status == "starting"):
		// The live instance is still starting; the caller may wait on it.
		return instance, StatusStarting, "", nil
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

// waitForCall waits for a call's live instance that is still starting and
// returns it with its status once the wait is over. err is the reason the
// wait ended early: a timeout, a disable or the session's end.
func (m *metaTools) waitForCall(ctx context.Context, request *mcp.CallToolRequest, current requestContext,
	template *v1alpha1.MCPServerTemplate, instance db.Instance,
) (db.Instance, string, string, error) {
	instance, err := m.waitForInstance(ctx, request, current, template, instance)
	if err != nil {
		return db.Instance{}, "", "", err
	}

	if instance.Status == statusReady && instance.Desired == "running" && !instance.FailedAt.Valid {
		return instance, statusReady, "", nil
	}

	status, reason := shownStatus(instance)
	if status == StatusReady {
		// Ready but no longer live: it is being stopped.
		status = StatusStopped
	}

	return db.Instance{}, status, reason, nil
}

// refuseCall writes the refusing tool.decision and returns message to the
// model as a tool error.
func (m *metaTools) refuseCall(ctx context.Context, current requestContext, server, tool string, details map[string]any, message string) (*mcp.CallToolResult, error) {
	details["decision"] = "refuse"
	withExposedDetail(ctx, details)
	observationOf(ctx).setOutcome(runtime.CallRefused)

	recorder := audit.NewRecorder(m.auditOutput, db.New(m.database))
	if err := recorder.Record(ctx, callEvent(current, "tool.decision", server, tool, details)); err != nil {
		return nil, err
	}

	return errorResult(message), nil
}

type exposedToolKey struct{}

// withExposedTool records the exposed name of the tool a call is about, for
// audit events of the call.
func withExposedTool(ctx context.Context, exposed string) context.Context {
	return context.WithValue(ctx, exposedToolKey{}, exposed)
}

// withExposedDetail adds `exposedTool` to details when the call's exposed
// name differs from its upstream name.
func withExposedDetail(ctx context.Context, details map[string]any) map[string]any {
	if exposed, _ := ctx.Value(exposedToolKey{}).(string); exposed != "" {
		details["exposedTool"] = exposed
	}

	return details
}

// policyRequest builds the policy request of a call of the upstream tool
// name tool. Rules match the exposed name too.
func (m *metaTools) policyRequest(current requestContext, template *v1alpha1.MCPServerTemplate, tool string, state policy.State) policy.Request {
	return toolPolicyRequest(current, template, tool, toolcatalog.ExposedName(template, tool), state)
}

// toolPolicyRequest builds the policy request about one tool, by its
// upstream and exposed names. For a built-in tool it adds the exact name a
// session sees, such as toolgate_allow, so rules naming it match.
func toolPolicyRequest(current requestContext, template *v1alpha1.MCPServerTemplate, upstream, exposed string, state policy.State) policy.Request {
	request := policy.Request{
		Identity: current.identity, Template: template, Tool: upstream,
		ExposedTool: exposed, State: state,
	}
	if proxied, ok := builtinProxiedName(template.Name, upstream); ok {
		request.ProxiedTool = proxied
	}

	return request
}

func callEvent(current requestContext, eventType, server, tool string, details map[string]any) audit.Event {
	event := auditEvent(current, eventType, server, details)
	event.Tool = tool

	return event
}

// messageNotRecorded is the refusal of a call whose arguments couldn't be
// stored.
const messageNotRecorded = "The call couldn't be recorded. Try again later."

// callArguments returns a call's arguments as an audit payload: nil when
// the call has none.
func callArguments(arguments json.RawMessage) any {
	if len(arguments) == 0 {
		return nil
	}

	return arguments
}

func errorResult(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}
}

// callRefusal is a refusal found while starting an instance for a call.
type callRefusal struct {
	details map[string]any
	message string
}

// startForCall is call step 5 for a session whose server has no live
// instance. For an `onCall` server with a catalog list it refuses when the
// newest instance failed on the template's current generation, and
// otherwise returns the live instance, inserting a pending one within the
// limits. Other servers (`onEnable`, or no catalog list) insert one only
// when the newest instance failed on an older generation, and otherwise
// return the zero instance and are handled as before.
func (m *metaTools) startForCall(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate) (db.Instance, *callRefusal, error) {
	lazy, err := m.startsOnCall(ctx, template)
	if err != nil {
		return db.Instance{}, nil, err
	}

	var instance db.Instance

	var refusal *callRefusal

	attempt := func() error {
		instance, refusal = db.Instance{}, nil

		return pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
			queries := db.New(tx)

			if err := m.lockSession(ctx, tx, current); err != nil {
				return err
			}

			enabled, err := m.isEnabled(ctx, queries, current.session.ID, template)
			if err != nil {
				return err
			}

			if !enabled {
				if !lazy {
					return nil
				}

				refusal = &callRefusal{
					details: map[string]any{"reason": reasonNotEnabled},
					message: fmt.Sprintf("Server `%s` isn't enabled in this session. Call `%s` first.", template.Name, ToolEnableServer),
				}

				return nil
			}

			key := scopeKey(template.Spec.Scope, current.identity)
			if err := queries.LockInstanceScope(ctx, db.LockInstanceScopeParams{
				TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: key,
			}); err != nil {
				return fmt.Errorf("lock instance scope: %w", err)
			}

			live, err := queries.GetLiveInstance(ctx, db.GetLiveInstanceParams{
				TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: key,
			})
			if err == nil {
				instance = live

				return nil
			}

			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read live instance: %w", err)
			}

			newest, err := queries.GetNewestInstance(ctx, db.GetNewestInstanceParams{
				TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: key,
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read newest instance: %w", err)
			}

			olderFailure := err == nil && newest.FailedAt.Valid && newest.Generation != template.Generation
			if !lazy && !olderFailure {
				return nil
			}

			if err == nil && newest.FailedAt.Valid && newest.Generation == template.Generation && newest.TemplateUid == string(template.UID) {
				reason := newest.Reason
				if reason == "" {
					reason = "it failed to start"
				}

				refusal = &callRefusal{
					details: map[string]any{"reason": reasonNotReady, "status": StatusFailed, "detail": newest.Reason},
					message: fmt.Sprintf("Server `%s` failed to start: %s Enable `%s` again to retry.", template.Name, sentence(reason), template.Name),
				}

				return nil
			}

			// A start renders the template, so an exchange in it needs a
			// user's token.
			if err := checkUserToken(current, template); err != nil {
				refusal = &callRefusal{details: map[string]any{"reason": reasonNoUserToken}, message: err.Error()}

				return nil
			}

			if err := m.checkInstanceLimits(ctx, queries, current, template); err != nil {
				return err
			}

			instance, err = insertInstance(ctx, queries, current, template, key, m.sharedNamespace)
			if err != nil {
				return err
			}

			return store.Notify(ctx, tx, store.KindInstance, instance.ID)
		})
	}

	err = attempt()
	if retryable(err) {
		err = attempt()
	}

	var limitErr *instanceLimitError
	if errors.As(err, &limitErr) {
		return db.Instance{}, &callRefusal{
			details: map[string]any{"reason": "limit", "limit": limitErr.limit},
			message: limitErr.message,
		}, nil
	}

	return instance, refusal, err
}

// sentence ends text with a full stop.
func sentence(text string) string {
	if strings.HasSuffix(text, ".") {
		return text
	}

	return text + "."
}

// instanceOffers reports whether the curated tool list of a ready instance
// holds the upstream tool.
func instanceOffers(instance db.Instance, template *v1alpha1.MCPServerTemplate, tool string) (bool, error) {
	var upstream []*mcp.Tool
	if len(instance.Tools) > 0 {
		if err := json.Unmarshal(instance.Tools, &upstream); err != nil {
			return false, fmt.Errorf("decode tools of instance %s: %w", instance.ID, err)
		}
	}

	for _, curated := range toolcatalog.Curate(template, upstream) {
		if curated.UpstreamName == tool {
			return true, nil
		}
	}

	return false, nil
}
