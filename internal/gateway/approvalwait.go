package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// WaitingCallLimit is the most calls of one principal that wait for an
// approval on one replica.
const WaitingCallLimit = 32

// approvalWaitInterval is the longest a waiting call goes without
// re-reading its approval and sending a progress notification. An e2e
// build scales it with the approval timeout.
var approvalWaitInterval = testtime.Scale(progressInterval)

// Approval statuses a waiting call ends on.
const approvalDenied = "denied"

const (
	reasonApprovalDenied = "approval-denied"
	elicitAccept         = "accept"
)

// approvalWaiters counts the calls of each principal that wait for an
// approval on this replica.
type approvalWaiters struct {
	mu      sync.Mutex
	waiting map[string]int
}

// acquire takes a waiting slot for principal. It returns false when the
// principal already has WaitingCallLimit waiting calls.
func (w *approvalWaiters) acquire(principal string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.waiting == nil {
		w.waiting = make(map[string]int)
	}

	if w.waiting[principal] >= WaitingCallLimit {
		return false
	}

	w.waiting[principal]++

	return true
}

// count returns how many calls of principal are waiting.
func (w *approvalWaiters) count(principal string) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.waiting[principal]
}

func (w *approvalWaiters) release(principal string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.waiting[principal]--
	if w.waiting[principal] <= 0 {
		delete(w.waiting, principal)
	}
}

// supportsURLElicitation reports whether the client declared URL
// elicitation, for the negotiated revision, and the call came through the
// go-sdk's tool path: the request's _meta on
// 2026-07-28, the initialize capabilities before it.
func supportsURLElicitation(ctx context.Context, request *mcp.CallToolRequest) bool {
	if !elicitationAllowed(ctx) {
		return false
	}

	capabilities := request.ClientCapabilities()

	return capabilities != nil && capabilities.Elicitation != nil && capabilities.Elicitation.URL != nil
}

// awaitApproval waits for an
// approval that isn't usable yet. It returns the approved approval for the
// use step, or a result that ends the call.
func (m *metaTools) awaitApproval(ctx context.Context, call approvalCall, found foundApproval) (db.Approval, *mcp.CallToolResult, error) {
	id := found.approval.ID
	responses := call.request.Params.InputResponses

	// Step 3: a client that can't show the page, an identity that isn't a
	// user (it can't sign in to the UI), or a request with responses whose
	// approval the user never saw.
	if !supportsURLElicitation(ctx, call.request) || call.current.identity.Kind != auth.KindUser ||
		(responses != nil && found.inserted) {
		result, err := m.refusePending(ctx, call, id)

		return db.Approval{}, result, err
	}

	// Step 4: ask the client to open the approval page. A request that
	// carries responses never gets input requests back.
	if responses == nil {
		return db.Approval{}, m.approvalInputRequest(call, id), nil
	}

	if !approvalAccepted(responses, id) {
		result, err := m.refusePending(ctx, call, id)

		return db.Approval{}, result, err
	}

	// Step 5.
	principal := call.current.identity.Principal
	if !m.waiters.acquire(principal) {
		result, err := m.refusePending(ctx, call, id)

		return db.Approval{}, result, err
	}
	defer m.waiters.release(principal)

	return m.waitForApproval(ctx, call, id)
}

// approvalInputRequest is a result
// asking the client, through the go-sdk's multi round-trip handling, to open
// the approval page. The elicitation ID and the request state are both the
// approval ID.
func (m *metaTools) approvalInputRequest(call approvalCall, id string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{id: &mcp.ElicitParams{
			Mode:          "url",
			Message:       fmt.Sprintf("Tool `%s` needs approval. Open the page to approve or deny the call.", call.proxiedName()),
			URL:           m.approvalURL(id),
			ElicitationID: id,
		}},
		RequestState: id,
	}
}

// approvalAccepted reports whether the client answered the approval's
// elicitation with accept.
func approvalAccepted(responses mcp.InputResponseMap, id string) bool {
	answer, ok := responses[id].(*mcp.ElicitResult)

	return ok && answer.Action == elicitAccept
}

// waitForApproval waits for the decision. It subscribes
// before it reads, then re-reads the approval and decides again on every
// approval:<id> notification, missed event and tick. A cancel or a
// disconnect ends the wait with ctx's error and writes nothing.
func (m *metaTools) waitForApproval(ctx context.Context, call approvalCall, id string) (db.Approval, *mcp.CallToolResult, error) {
	events, unsubscribe := m.subscribe()
	defer unsubscribe()

	ticker := time.NewTicker(approvalWaitInterval)
	defer ticker.Stop()

	progress := progressSender{request: call.request}
	queries := db.New(m.database)

	for {
		row, err := queries.GetApprovalForWait(ctx, id)
		if err != nil {
			return db.Approval{}, nil, waitError(ctx, fmt.Errorf("read approval: %w", err))
		}

		progress.send(ctx, fmt.Sprintf("Waiting for approval `%s`: it is %s", id, row.Approval.Status))

		if result, err := m.decideWhileWaiting(ctx, call, id); result != nil || err != nil {
			return db.Approval{}, result, waitError(ctx, err)
		}

		switch {
		case row.Approval.Status == approvalApproved:
			return row.Approval, nil, nil
		case row.Approval.Status == approvalDenied:
			result, err := m.refuseCall(ctx, call.current, call.server(), call.tool,
				map[string]any{"reason": reasonApprovalDenied, "approval": id},
				fmt.Sprintf("Approval `%s` was denied.", id))

			return db.Approval{}, result, err
		case row.Approval.Status == approvalExpired:
			// The leader's pass expired it and wrote approval.expire.
			result, err := m.refuseTimeout(ctx, call, id)

			return db.Approval{}, result, err
		case row.Approval.Status == approvalPending && row.Overdue:
			won, err := m.expireApproval(ctx, call, id)
			if err != nil {
				return db.Approval{}, nil, waitError(ctx, err)
			}

			if won {
				result, err := m.refuseTimeout(ctx, call, id)

				return db.Approval{}, result, err
			}

			// Someone decided first: re-read at once.
			continue
		}

		if err := awaitApprovalWake(ctx, events, ticker.C, id); err != nil {
			return db.Approval{}, nil, err
		}
	}
}

// waitError returns ctx's error when the wait ended because the client
// cancelled or disconnected, so nothing is reported as a gateway failure.
func waitError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return err
}

// awaitApprovalWake blocks until a notification about the approval
// arrives, notifications were missed, or the tick fires.
func awaitApprovalWake(ctx context.Context, events <-chan store.Event, tick <-chan time.Time, id string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
			return nil
		case event := <-events:
			if event.Missed || (event.Kind == store.KindApproval && event.ID == id) {
				return nil
			}
		}
	}
}

// decideWhileWaiting decides steps 1 to 4
// again with fresh policy state. A refusal ends the wait.
func (m *metaTools) decideWhileWaiting(ctx context.Context, call approvalCall, id string) (*mcp.CallToolResult, error) {
	template := call.template
	if running, found := m.catalog.Running(types.NamespacedName{Namespace: template.Namespace, Name: template.Name}); found {
		template = running
	}

	state, err := m.policyState(ctx, call.current.identity)
	if err != nil {
		return nil, err
	}

	decision := m.decide(ctx, m.policyRequest(call.current, template, call.tool, state))
	if decision.Allowed {
		return nil, nil
	}

	details := refusalDetails(decision)
	details["approval"] = id

	return m.refuseCall(ctx, call.current, call.server(), call.tool, details, decision.Reason)
}

// expireApproval compare-and-sets the overdue approval from pending to
// expired and, only when it wins, writes approval.expire and notifies
// approval:<id>.
func (m *metaTools) expireApproval(ctx context.Context, call approvalCall, id string) (bool, error) {
	won := false

	err := pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if _, err := queries.ExpireDueApproval(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}

			return fmt.Errorf("expire approval: %w", err)
		}

		won = true

		event := callEvent(call.current, "approval.expire", call.server(), call.tool, map[string]any{"approval": id})
		if err := audit.NewRecorder(m.auditOutput, queries).Record(ctx, event); err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindApproval, id)
	})
	if err == nil && won {
		opsmetrics.ApprovalOutcome(opsmetrics.KindTool, opsmetrics.OutcomeExpired, 1)
	}

	if err != nil {
		return false, err
	}

	return won, nil
}

// refuseTimeout writes the approval-timeout refusal and returns the
// timeout message.
func (m *metaTools) refuseTimeout(ctx context.Context, call approvalCall, id string) (*mcp.CallToolResult, error) {
	return m.refuseCall(ctx, call.current, call.server(), call.tool,
		map[string]any{"reason": reasonApprovalTimeout, "approval": id}, m.timeoutMessage(call, id))
}
