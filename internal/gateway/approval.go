package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// PendingApprovalLimit is the most pending approvals one principal may
// have (docs/design/phase-2.md §2, "Approvals").
const PendingApprovalLimit = 100

// DefaultApprovalTimeout is the approvalTimeout when MCPOptions sets none.
const DefaultApprovalTimeout = 10 * time.Minute

// Refusal reasons of calls that need approval (docs/design/phase-2.md §4).
const (
	reasonApprovalPending = "approval-pending"
	reasonApprovalTimeout = "approval-timeout"
	reasonApprovalLimit   = "approval-limit"
	reasonBadArguments    = "invalid-arguments"
)

// Approval statuses (the approvals.status column).
const (
	approvalPending  = "pending"
	approvalApproved = "approved"
	approvalExpired  = "expired"
)

// canonicalArguments returns the fixed form of a call's arguments and its
// SHA-256: decoded with encoding/json using UseNumber, then encoded again,
// which sorts object keys and keeps numbers as written. Empty arguments stay
// empty.
func canonicalArguments(arguments json.RawMessage) (json.RawMessage, []byte, error) {
	if len(bytes.TrimSpace(arguments)) == 0 {
		sum := sha256.Sum256(nil)

		return nil, sum[:], nil
	}

	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, err
	}

	if decoder.More() {
		return nil, nil, errors.New("more than one JSON value")
	}

	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}

	sum := sha256.Sum256(canonical)

	return canonical, sum[:], nil
}

// approvalCall is one call of a tool that needs approval, with what step 1
// of docs/design/phase-2.md §4 looks approvals up by.
type approvalCall struct {
	current   requestContext
	request   *mcp.CallToolRequest
	template  *v1alpha1.MCPServerTemplate
	tool      string
	arguments json.RawMessage
	hash      []byte
}

func (c approvalCall) server() string { return c.template.Name }

func (c approvalCall) proxiedName() string { return ProxiedToolName(c.template.Name, c.tool) }

// callWithApproval runs a call whose decision has NeedsApproval
// (docs/design/phase-2.md §4). It replaces phase 1's call steps 3 and 4.
func (m *metaTools) callWithApproval(ctx context.Context, current requestContext, request *mcp.CallToolRequest, template *v1alpha1.MCPServerTemplate, tool string) (*mcp.CallToolResult, error) {
	server := template.Name

	if m.approvalURL == nil {
		return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonApprovalPending},
			fmt.Sprintf("Tool `%s` needs approval, but this gateway has no web UI. Ask an admin.", ProxiedToolName(server, tool)))
	}

	arguments, hash, err := canonicalArguments(request.Params.Arguments)
	if err != nil {
		return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonBadArguments},
			fmt.Sprintf("The arguments of `%s` aren't valid JSON.", ProxiedToolName(server, tool)))
	}

	call := approvalCall{current: current, request: request, template: template, tool: tool, arguments: arguments, hash: hash}

	// Step 6 starts again at step 1 once when another call used the
	// approval first.
	for attempt := 0; ; attempt++ {
		found, result, err := m.findOrRequestApproval(ctx, call)
		if result != nil || err != nil {
			return result, err
		}

		approval := found.approval

		if !found.usable {
			approval, result, err = m.awaitApproval(ctx, call, found)
			if result != nil || err != nil {
				return result, err
			}
		}

		result, used, err := m.useApproval(ctx, call, approval)
		if used || err != nil || attempt > 0 {
			if !used && err == nil {
				return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonApprovalPending, "approval": approval.ID},
					fmt.Sprintf("Approval `%s` was used by another call. Call `%s` again.", approval.ID, call.proxiedName()))
			}

			return result, err
		}
	}
}

// refusePending is step 3 of docs/design/phase-2.md §4: the call fails
// with a link to the approval, which stays pending for a retry.
func (m *metaTools) refusePending(ctx context.Context, call approvalCall, id string) (*mcp.CallToolResult, error) {
	return m.refuseCall(ctx, call.current, call.server(), call.tool,
		map[string]any{"reason": reasonApprovalPending, "approval": id},
		fmt.Sprintf("Tool `%s` needs approval. Approve request `%s` at %s, then call the tool again with the same arguments.",
			call.proxiedName(), id, m.approvalURL(id)))
}

// foundApproval is the approval step 1 of docs/design/phase-2.md §4 found
// or inserted for a call.
type foundApproval struct {
	approval db.Approval
	usable   bool
	// inserted is true when step 1 inserted the approval, so the user
	// hasn't seen it yet.
	inserted bool
}

// findOrRequestApproval is step 1 of docs/design/phase-2.md §4: it finds
// the call's approval, in the order the one named by RequestState, a usable
// approved one, a pending one, or inserts a pending one. A non-nil result
// ends the call.
func (m *metaTools) findOrRequestApproval(ctx context.Context, call approvalCall) (foundApproval, *mcp.CallToolResult, error) {
	queries := db.New(m.database)

	if id := call.request.Params.RequestState; id != "" {
		row, err := queries.GetMatchingApproval(ctx, db.GetMatchingApprovalParams{
			ID: id, Principal: call.current.identity.Principal, SessionPk: call.current.session.ID,
			TemplateNs: call.template.Namespace, TemplateName: call.template.Name, Tool: call.tool, ArgumentsSha256: call.hash,
		})

		switch {
		case err == nil && row.Approval.Status == approvalExpired:
			result, err := m.refuseTimeout(ctx, call, id)

			return foundApproval{}, result, err
		case err == nil && (row.Usable || row.Approval.Status == approvalPending):
			return foundApproval{approval: row.Approval, usable: row.Usable}, nil, nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return foundApproval{}, nil, fmt.Errorf("read approval: %w", err)
		}
	}

	find := db.FindUsableApprovalParams{
		Principal: call.current.identity.Principal, SessionPk: call.current.session.ID,
		TemplateNs: call.template.Namespace, TemplateName: call.template.Name, Tool: call.tool, ArgumentsSha256: call.hash,
	}

	approval, err := queries.FindUsableApproval(ctx, find)
	if err == nil {
		return foundApproval{approval: approval, usable: true}, nil, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return foundApproval{}, nil, fmt.Errorf("read approval: %w", err)
	}

	approval, err = queries.FindPendingApproval(ctx, db.FindPendingApprovalParams(find))
	if err == nil {
		return foundApproval{approval: approval}, nil, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return foundApproval{}, nil, fmt.Errorf("read approval: %w", err)
	}

	return m.requestApproval(ctx, call)
}

// errApprovalLimit stops the insert of an approval past the principal's
// pending limit.
var errApprovalLimit = errors.New("pending approval limit")

// requestApproval inserts a pending approval and writes approval.request
// with the canonical arguments as its payload, in one transaction.
func (m *metaTools) requestApproval(ctx context.Context, call approvalCall) (foundApproval, *mcp.CallToolResult, error) {
	generated, err := uuid.NewV7()
	if err != nil {
		return foundApproval{}, nil, fmt.Errorf("approval ID: %w", err)
	}

	id := hex.EncodeToString(generated[:])

	event := callEvent(call.current, "approval.request", call.server(), call.tool, map[string]any{"approval": id})
	if err := m.payloads.Attach(ctx, &event, callArguments(call.arguments)); err != nil {
		result, err := m.refuseCall(ctx, call.current, call.server(), call.tool,
			map[string]any{"reason": reasonPayloadError, "payload_error": err.Error()}, messageNotRecorded)

		return foundApproval{}, result, err
	}

	var found foundApproval

	err = pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if err := queries.LockPrincipalApprovals(ctx, call.current.identity.Principal); err != nil {
			return fmt.Errorf("lock approvals: %w", err)
		}

		// A call with the same arguments may have inserted one while this
		// one looked; the lock orders the two.
		pending, err := queries.FindPendingApproval(ctx, db.FindPendingApprovalParams{
			Principal: call.current.identity.Principal, SessionPk: call.current.session.ID,
			TemplateNs: call.template.Namespace, TemplateName: call.template.Name, Tool: call.tool, ArgumentsSha256: call.hash,
		})
		if err == nil {
			found.approval = pending

			return nil
		}

		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read approval: %w", err)
		}

		found.approval, err = queries.InsertApprovalWithinLimit(ctx, db.InsertApprovalWithinLimitParams{
			ID: id, Principal: call.current.identity.Principal, SessionPk: call.current.session.ID,
			Workspace: call.current.identity.Workspace, TemplateNs: call.template.Namespace, TemplateName: call.template.Name,
			Tool: call.tool, ArgumentsSha256: call.hash,
			ApprovalTimeout: pgtype.Interval{Microseconds: testtime.Scale(m.approvalTimeout).Microseconds(), Valid: true},
			PendingLimit:    PendingApprovalLimit,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errApprovalLimit
		}

		if err != nil {
			return fmt.Errorf("insert approval: %w", err)
		}

		found.inserted = true

		if err := audit.NewRecorder(m.auditOutput, queries).Record(ctx, event); err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindApproval, id)
	})

	if errors.Is(err, errApprovalLimit) {
		result, err := m.refuseCall(ctx, call.current, call.server(), call.tool, map[string]any{"reason": reasonApprovalLimit},
			fmt.Sprintf("You have %d approvals waiting. Approve or deny some first.", PendingApprovalLimit))

		return foundApproval{}, result, err
	}

	if err != nil {
		return foundApproval{}, nil, err
	}

	return found, nil, nil
}

// beforeApprovalUse, when set, runs before step 6 begins. Tests use it to
// change state while a call holds a usable approval, as a wait would.
var beforeApprovalUse func()

// errRefusedInUse rolls back the use of an approval when a check refuses.
var errRefusedInUse = errors.New("refused")

// useApproval is step 6 of docs/design/phase-2.md §4. In one transaction it
// checks the server is still enabled and its live instance ready, uses the
// approval, decides again with fresh policy state and writes the allow;
// then it forwards the canonical arguments. Any refusal rolls back, so the
// approval stays usable. used is false when another call used the approval
// first.
func (m *metaTools) useApproval(ctx context.Context, call approvalCall, approval db.Approval) (*mcp.CallToolResult, bool, error) {
	var (
		instance db.Instance
		template *v1alpha1.MCPServerTemplate
		refusal  map[string]any
		message  string
		lost     bool
	)

	if beforeApprovalUse != nil {
		beforeApprovalUse()
	}

	allow := callEvent(call.current, "tool.decision", call.server(), call.tool, map[string]any{"decision": "allow", "approval": approval.ID})
	if err := m.payloads.Attach(ctx, &allow, callArguments(call.arguments)); err != nil {
		result, err := m.refuseCall(ctx, call.current, call.server(), call.tool,
			map[string]any{"reason": reasonPayloadError, "payload_error": err.Error(), "approval": approval.ID}, messageNotRecorded)

		return result, true, err
	}

	err := pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		var enabled bool

		template, enabled, refusal, message = m.enabledInTx(ctx, queries, call)
		if !enabled {
			return errRefusedInUse
		}

		var err error

		instance, refusal, message, err = m.readyInTx(ctx, queries, call, template)
		if err != nil {
			return err
		}

		if refusal != nil {
			return errRefusedInUse
		}

		if _, err := queries.UseApproval(ctx, approval.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				lost = true

				return errRefusedInUse
			}

			return fmt.Errorf("use approval: %w", err)
		}

		state, err := m.policyState(ctx, call.current.identity)
		if err != nil {
			return err
		}

		decision := m.decide(ctx, m.policyRequest(call.current, template, call.tool, state))
		if !decision.Allowed {
			refusal, message = refusalDetails(decision), decision.Reason

			return errRefusedInUse
		}

		allow.Details["instance"] = instance.ID
		if decision.Grant != "" {
			allow.Details["grant"] = decision.Grant
		}

		return audit.NewRecorder(m.auditOutput, queries).Record(ctx, allow)
	})

	switch {
	case lost:
		return nil, false, nil
	case errors.Is(err, errRefusedInUse):
		refusal["approval"] = approval.ID
		result, err := m.refuseCall(ctx, call.current, call.server(), call.tool, refusal, message)

		return result, true, err
	case err != nil:
		return nil, true, fmt.Errorf("The call wasn't forwarded: the gateway can't record it. Try again later. (%w)", err) //nolint:staticcheck // A sentence for the model.
	}

	result, err := m.forwardAndRecord(ctx, call.current, instance, template, call.tool, call.arguments)

	return result, true, err
}

// enabledInTx re-runs phase 1 call step 1 inside the use transaction: the
// session must still have the server enabled.
func (m *metaTools) enabledInTx(ctx context.Context, queries *db.Queries, call approvalCall) (*v1alpha1.MCPServerTemplate, bool, map[string]any, string) {
	server := call.server()

	rows, err := queries.ListSessionServers(ctx, call.current.session.ID)
	if err == nil {
		for _, row := range rows {
			if row.TemplateNs != call.template.Namespace || row.TemplateName != server {
				continue
			}

			if template, found := m.catalog.Running(types.NamespacedName{Namespace: row.TemplateNs, Name: row.TemplateName}); found {
				return template, true, nil, ""
			}

			// The template is gone: the server stays enabled with no
			// instance.
			template := &v1alpha1.MCPServerTemplate{}
			template.Namespace, template.Name = row.TemplateNs, row.TemplateName

			return template, true, nil, ""
		}
	}

	return nil, false, map[string]any{"reason": reasonNotEnabled},
		fmt.Sprintf("Server `%s` isn't enabled in this session. Call `%s` first.", server, ToolEnableServer)
}

// readyInTx re-runs phase 1 call step 2 inside the use transaction: it
// re-selects the live instance, which must be ready.
func (m *metaTools) readyInTx(ctx context.Context, queries *db.Queries, call approvalCall, template *v1alpha1.MCPServerTemplate) (db.Instance, map[string]any, string, error) {
	server := template.Name

	notReady := func(status, reason string) (db.Instance, map[string]any, string, error) {
		message := fmt.Sprintf("Server `%s` isn't ready: it is %s.", server, status)
		if reason != "" {
			message = fmt.Sprintf("Server `%s` isn't ready: it is %s: %s", server, status, reason)
		}

		return db.Instance{}, map[string]any{"reason": reasonNotReady, "status": status, "detail": reason}, message, nil
	}

	if template.UID == "" {
		return notReady(StatusStopped, "the server was removed from the catalog")
	}

	instance, err := queries.GetLiveInstance(ctx, db.GetLiveInstanceParams{
		TemplateNs: template.Namespace, TemplateName: template.Name,
		ScopeKey: scopeKey(template.Spec.Scope, call.current.identity),
	})
	if err == nil && instance.Status == statusReady {
		if instance.UnhealthySince.Valid {
			return db.Instance{}, map[string]any{"reason": reasonNotReady, "status": statusReady, "detail": instance.UnhealthyReason},
				unhealthyMessage(server, instance), nil
		}

		return instance, nil, "", nil
	}

	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return db.Instance{}, nil, "", fmt.Errorf("read live instance: %w", err)
	}

	// The same status and reason the call path shows.
	_, status, reason, err := m.readyInstance(ctx, call.current, template)
	if err != nil {
		return db.Instance{}, nil, "", err
	}

	if status == statusReady {
		// Ready again since the read above: still refuse, so the retry
		// reads it in its own transaction.
		status = StatusStarting
	}

	return notReady(status, reason)
}

// timeoutMessage is the refusal of a call whose approval expired.
func (m *metaTools) timeoutMessage(call approvalCall, id string) string {
	return fmt.Sprintf("Nobody approved `%s` within %s (approval `%s`).", call.proxiedName(), durationWords(m.approvalTimeout), id)
}

// durationWords renders a whole number of minutes or hours in words, such as
// "10 minutes" or "1 hour".
func durationWords(duration time.Duration) string {
	unit := func(count int64, name string) string {
		if count == 1 {
			return "1 " + name
		}

		return fmt.Sprintf("%d %ss", count, name)
	}

	switch {
	case duration > 0 && duration%time.Hour == 0:
		return unit(int64(duration/time.Hour), "hour")
	case duration > 0 && duration%time.Minute == 0:
		return unit(int64(duration/time.Minute), "minute")
	default:
		return strings.TrimSpace(duration.String())
	}
}
