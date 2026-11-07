package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// Refusal reasons of an enable that asked for a session addition.
const (
	reasonSessionEnded     = "session-ended"
	reasonAdditionDenied   = "addition-denied"
	reasonAdditionTimeout  = "addition-timeout"
	reasonAdditionPending  = "addition-pending"
	reasonAdditionLimit    = "addition-limit"
	typeAccessExpire       = "access.expire"
	sessionAdditionRequest = "request"
)

// additionEnable is one enable that asks for a session addition.
type additionEnable struct {
	current  requestContext
	request  *mcp.CallToolRequest
	template *v1alpha1.MCPServerTemplate
	// tool is the upstream tool name when a call, not an enable, asks. The
	// refusals are then written as tool.decision.
	tool string
}

// additionRefusal is a refusal of an enable or call that asked for a session
// addition. It has been recorded.
type additionRefusal struct{ message string }

func (e *additionRefusal) Error() string { return e.message }

// record writes the event of a refusal: server.enable for an enable,
// tool.decision for a call.
func (e additionEnable) record(ctx context.Context, recorder *audit.Recorder, details map[string]any) error {
	if e.tool != "" {
		return recorder.Record(ctx, callEvent(e.current, "tool.decision", e.server(), e.tool, withExposedDetail(ctx, details)))
	}

	return recorder.Record(ctx, auditEvent(e.current, "server.enable", e.server(), details))
}

func (e additionEnable) server() string { return e.template.Name }

func (e additionEnable) workspace() string { return VerifiedWorkspace(e.current.identity) }

// asksForAddition reports whether an enable refused only by the
// workspace-scope step asks the user: only in a verified workspace, only
// on a gateway with the web UI, and never for a deny entry's refusal.
func (m *metaTools) asksForAddition(current requestContext, decision policy.Decision) bool {
	return !decision.Allowed && decision.Step == policy.StepWorkspaceScope && decision.Denied == "" &&
		VerifiedWorkspace(current.identity) != "" && m.approvalURL != nil
}

// requestSessionAddition runs the policy steps for an enable whose
// only refusal is the workspace-scope step. It returns
// the request's ID when the enable may go on to the instance steps. A
// non-nil result or error ends the enable; a refusal error has been
// recorded, a context error (cancel or disconnect) has not.
func (m *metaTools) requestSessionAddition(ctx context.Context, enable additionEnable) (string, *mcp.CallToolResult, error) {
	found, err := m.findOrInsertServerRequest(ctx, enable)
	if err != nil {
		return "", nil, err
	}

	id := found.request.ID
	responses := enable.request.Params.InputResponses

	// Step 2: a client that can't show the page, or responses for a request
	// the user never saw.
	if !supportsURLElicitation(ctx, enable.request) || (responses != nil && found.inserted) {
		return "", nil, m.refuseAdditionPending(ctx, enable, id)
	}

	// Step 3: ask the client to open the approval page. A request that
	// carries responses never gets input requests back.
	if responses == nil {
		return "", m.additionInputRequest(enable, id), nil
	}

	if !approvalAccepted(responses, id) {
		return "", nil, m.refuseAdditionPending(ctx, enable, id)
	}

	// Step 4, counted with the calls waiting for an approval.
	principal := enable.current.identity.Principal
	if !m.waiters.acquire(principal) {
		return "", nil, m.refuseAdditionPending(ctx, enable, id)
	}
	defer m.waiters.release(principal)

	if err := m.waitForAddition(ctx, enable, id); err != nil {
		return "", nil, waitError(ctx, err)
	}

	return id, nil, nil
}

// foundServerRequest is the request step 1 found or inserted.
type foundServerRequest struct {
	request db.AccessRequest
	// inserted is true when step 1 inserted it, so the user hasn't seen it.
	inserted bool
}

// findOrInsertServerRequest is step 1: the request named by RequestState
// when every field matches, else a pending request for this session and
// server, else a new one. A named request that was denied or expired ends
// the enable with its refusal.
func (m *metaTools) findOrInsertServerRequest(ctx context.Context, enable additionEnable) (foundServerRequest, error) {
	queries := db.New(m.database)
	principal := enable.current.identity.Principal
	sessionPK := pgtype.Int8{Int64: enable.current.session.ID, Valid: true}
	server := pgtype.Text{String: enable.server(), Valid: true}

	if id := enable.request.Params.RequestState; id != "" {
		row, err := queries.GetMatchingServerRequest(ctx, db.GetMatchingServerRequestParams{
			ID: id, Principal: principal, SessionPk: sessionPK, Server: server, Workspace: enable.workspace(),
		})

		switch {
		case err == nil && row.AccessRequest.Status == accessDenied:
			return foundServerRequest{}, m.refuseAddition(ctx, enable, reasonAdditionDenied, id, deniedAdditionMessage(enable, id))
		case err == nil && row.AccessRequest.Status == accessExpired:
			return foundServerRequest{}, m.refuseAddition(ctx, enable, reasonAdditionTimeout, id, m.additionTimeoutMessage(enable, id))
		case err == nil && row.AccessRequest.Status == accessPending:
			return foundServerRequest{request: row.AccessRequest}, nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return foundServerRequest{}, fmt.Errorf("read access request: %w", err)
		}
	}

	find := db.FindPendingServerRequestParams{Principal: principal, SessionPk: sessionPK, Server: server, Workspace: enable.workspace()}

	request, err := queries.FindPendingServerRequest(ctx, find)
	if err == nil {
		return foundServerRequest{request: request}, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return foundServerRequest{}, fmt.Errorf("read access request: %w", err)
	}

	return m.insertServerRequest(ctx, enable, find)
}

// errAccessRequestLimit stops the insert of a request past the principal's
// pending limit.
var errAccessRequestLimit = errors.New("pending access request limit")

// insertServerRequest inserts a pending server request within the
// principal's pending limit, writes access.request and notifies
// approval:<id>, in one transaction.
func (m *metaTools) insertServerRequest(ctx context.Context, enable additionEnable, find db.FindPendingServerRequestParams) (foundServerRequest, error) {
	generated, err := uuid.NewV7()
	if err != nil {
		return foundServerRequest{}, fmt.Errorf("access request ID: %w", err)
	}

	id := hex.EncodeToString(generated[:])

	var found foundServerRequest

	err = pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if err := queries.LockPrincipalAccessRequests(ctx, find.Principal); err != nil {
			return fmt.Errorf("lock access requests: %w", err)
		}

		// Another enable of the same server may have inserted one while
		// this one looked; the lock orders the two.
		pending, err := queries.FindPendingServerRequest(ctx, find)
		if err == nil {
			found.request = pending

			return nil
		}

		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read access request: %w", err)
		}

		found.request, err = queries.InsertServerRequestWithinLimit(ctx, db.InsertServerRequestWithinLimitParams{
			ID: id, Principal: find.Principal, SessionPk: find.SessionPk, Workspace: find.Workspace, Server: find.Server,
			ApprovalTimeout: pgtype.Interval{Microseconds: testtime.Scale(m.approvalTimeout).Microseconds(), Valid: true},
			PendingLimit:    MaxPendingAccessRequestsPerPrincipal,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAccessRequestLimit
		}

		if err != nil {
			return fmt.Errorf("insert access request: %w", err)
		}

		found.inserted = true

		if err := audit.NewRecorder(m.auditOutput, queries).Record(ctx, accessEvent(enable.current.identity, TypeAccessRequest, found.request)); err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindApproval, id)
	})

	if errors.Is(err, errAccessRequestLimit) {
		return foundServerRequest{}, m.refuseAddition(ctx, enable, reasonAdditionLimit, "",
			fmt.Sprintf("You already have %d pending access requests. Approve or deny some first.", MaxPendingAccessRequestsPerPrincipal))
	}

	if err != nil {
		return foundServerRequest{}, err
	}

	return found, nil
}

// additionInputRequest is step 3: a result asking the client to open the
// approval page. The elicitation ID and the request state are both the
// request ID.
func (m *metaTools) additionInputRequest(enable additionEnable, id string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{id: &mcp.ElicitParams{
			Mode: "url",
			Message: fmt.Sprintf("Server `%s` isn't allowed in workspace `%s`. Open the page to allow or deny it.",
				enable.server(), enable.workspace()),
			URL:           m.approvalURL(id),
			ElicitationID: id,
		}},
		RequestState: id,
	}
}

// refuseAdditionPending is step 2: the enable fails with a link to the
// request, which stays pending.
func (m *metaTools) refuseAdditionPending(ctx context.Context, enable additionEnable, id string) error {
	return m.refuseAddition(ctx, enable, reasonAdditionPending, id, fmt.Sprintf(
		"Server `%s` isn't allowed in workspace `%s`. Allow it at %s, then enable it again.",
		enable.server(), enable.workspace(), m.approvalURL(id)))
}

// refuseAddition writes server.enable (refuse, workspace-scope) with reason
// and the request ID, and returns message as the enable's error.
func (m *metaTools) refuseAddition(ctx context.Context, enable additionEnable, reason, id, message string) error {
	details := map[string]any{"decision": "refuse", "step": policy.StepWorkspaceScope, "reason": reason}
	if id != "" {
		details[sessionAdditionRequest] = id
	}

	recorder := audit.NewRecorder(m.auditOutput, db.New(m.database))
	if err := enable.record(ctx, recorder, details); err != nil {
		return err
	}

	return &additionRefusal{message: message}
}

func deniedAdditionMessage(enable additionEnable, id string) string {
	return fmt.Sprintf("Adding `%s` was denied (request `%s`).", enable.server(), id)
}

func (m *metaTools) additionTimeoutMessage(enable additionEnable, id string) string {
	return fmt.Sprintf("Nobody allowed `%s` within %s (request `%s`).", enable.server(), durationWords(m.approvalTimeout), id)
}

// waitForAddition is step 4. It subscribes before it reads, then on every
// approval:<id>, scope:<id> and session:<id> notification, missed event
// and tick re-reads the session's ended_at and the request and decides
// steps 1 to 4 again. It returns nil once step 4 passes; every other
// outcome but a cancel is recorded and returned as the error.
func (m *metaTools) waitForAddition(ctx context.Context, enable additionEnable, id string) error {
	events, unsubscribe := m.subscribe()
	defer unsubscribe()

	ticker := time.NewTicker(approvalWaitInterval)
	defer ticker.Stop()

	progress := progressSender{request: enable.request}
	queries := db.New(m.database)
	sessionID := strconv.FormatInt(enable.current.session.ID, 10)

	for {
		scopeID := ""
		if scope, err := queries.GetUserScope(ctx, enable.current.identity.Principal); err == nil {
			scopeID = strconv.FormatInt(scope.ID, 10)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read user scope: %w", err)
		}

		done, err := m.decideAddition(ctx, enable, id, progress.send)
		if done || err != nil {
			return err
		}

		if err := awaitAdditionWake(ctx, events, ticker.C, id, sessionID, scopeID); err != nil {
			return err
		}
	}
}

// decideAddition is one wake of the wait. done is true with a nil error
// when step 4 passes.
func (m *metaTools) decideAddition(ctx context.Context, enable additionEnable, id string, report func(context.Context, string)) (bool, error) {
	queries := db.New(m.database)

	session, err := queries.GetSessionByID(ctx, enable.current.session.ID)
	if err != nil {
		return false, fmt.Errorf("read session: %w", err)
	}

	row, err := queries.GetAccessRequestForWait(ctx, id)
	if err != nil {
		return false, fmt.Errorf("read access request: %w", err)
	}

	request := row.AccessRequest

	// Ending a session expires its pending requests before their time, so
	// such a request means the session ended even when it restarted since.
	endedEarly := request.Status == accessExpired && !row.Overdue
	if session.EndedAt.Valid || endedEarly {
		return true, m.refuseAddition(ctx, enable, reasonSessionEnded, id,
			fmt.Sprintf("This session ended while waiting for `%s`.", enable.server()))
	}

	report(ctx, fmt.Sprintf("Waiting for request `%s`: it is %s", id, request.Status))

	template := enable.template
	if running, found := m.catalog.Running(types.NamespacedName{Namespace: template.Namespace, Name: template.Name}); found {
		template = running
	}

	state, err := m.policyState(ctx, enable.current.identity)
	if err != nil {
		return false, err
	}

	decision := m.decide(ctx, m.additionPolicyRequest(enable, template, state))

	switch {
	case decision.Allowed:
		return true, nil
	case decision.Step != policy.StepWorkspaceScope:
		details := refusalDetails(decision)
		details[sessionAdditionRequest] = id

		recorder := audit.NewRecorder(m.auditOutput, queries)
		if err := enable.record(ctx, recorder, details); err != nil {
			return true, err
		}

		return true, &additionRefusal{message: decision.Reason}
	}

	switch {
	case request.Status == accessDenied:
		return true, m.refuseAddition(ctx, enable, reasonAdditionDenied, id, deniedAdditionMessage(enable, id))
	case request.Status == accessExpired:
		return true, m.refuseAddition(ctx, enable, reasonAdditionTimeout, id, m.additionTimeoutMessage(enable, id))
	case request.Status == accessApproved:
		// Approved, but still refused, such as by a deny entry, which a
		// server approval never deletes.
		reason := policy.StepWorkspaceScope
		if decision.Denied != "" {
			reason = policy.ReasonDenied
		}

		return true, m.refuseAddition(ctx, enable, reason, id, decision.Reason)
	case row.Overdue:
		won, err := m.expireServerRequest(ctx, enable, request)
		if err != nil {
			return true, err
		}

		if won {
			return true, m.refuseAddition(ctx, enable, reasonAdditionTimeout, id, m.additionTimeoutMessage(enable, id))
		}

		// Someone decided first: re-read at once.
		return m.decideAddition(ctx, enable, id, report)
	}

	return false, nil
}

// expireServerRequest compare-and-sets the overdue request from pending to
// expired and, only when it wins, writes access.expire and notifies
// approval:<id>.
func (m *metaTools) expireServerRequest(ctx context.Context, enable additionEnable, request db.AccessRequest) (bool, error) {
	return m.expireAccessRequest(ctx, enable.current.identity, request.ID)
}

// awaitAdditionWake blocks until a notification about the request, the
// principal's scoping rows or the session arrives, notifications were
// missed, or the tick fires. With no user_scopes row yet, scopeID is empty
// and any scope notification wakes the wait, since the row's first write
// creates it.
func awaitAdditionWake(ctx context.Context, events <-chan store.Event, tick <-chan time.Time, id, sessionID, scopeID string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
			return nil
		case event := <-events:
			if event.Missed ||
				(event.Kind == store.KindApproval && event.ID == id) ||
				(event.Kind == store.KindSession && event.ID == sessionID) ||
				(event.Kind == store.KindScope && (scopeID == "" || event.ID == scopeID)) {
				return nil
			}
		}
	}
}

// additionPolicyRequest is the policy request the wait decides again: the
// server's for an enable, the call's tool's for a call.
func (m *metaTools) additionPolicyRequest(enable additionEnable, template *v1alpha1.MCPServerTemplate, state policy.State) policy.Request {
	if enable.tool != "" {
		return m.policyRequest(enable.current, template, enable.tool, state)
	}

	return policy.Request{Identity: enable.current.identity, Template: template, State: state}
}
