package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// AllowResult is the toolgate_allow result: the scope request's status
// (approved, denied, expired or pending), its ID and its approval page.
type AllowResult struct {
	Status  string `json:"status"`
	Request string `json:"request"`
	URL     string `json:"url"`
}

// parseScopeToolInput decodes and checks the input of toolgate_allow and
// toolgate_deny: exactly one target and 1 to 32 items. A non-empty message
// refuses the input.
func parseScopeToolInput(arguments json.RawMessage) (scopeToolInput, string) {
	var input scopeToolInput
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &input); err != nil {
			return input, "The input is `workspace` or `session`, and `servers`, a list of names."
		}
	}

	if (input.Workspace == "") == (input.Session == "") {
		return input, "Name exactly one of `workspace` and `session`."
	}

	if len(input.Servers) == 0 || len(input.Servers) > MaxScopeToolItems {
		return input, fmt.Sprintf("Name 1 to %d servers or tools.", MaxScopeToolItems)
	}

	return input, ""
}

// allowScope runs toolgate_allow: it finds or
// inserts the caller's scope request through the same method as the API,
// then asks the client to open the approval page and waits for the
// decision. Nothing changes until the user approves the request with a
// step-up on the page. A non-nil error is a cancel or a disconnect, which
// writes nothing.
func (m *metaTools) allowScope(ctx context.Context, current requestContext, request *mcp.CallToolRequest, arguments json.RawMessage) (*mcp.CallToolResult, string, error) {
	input, refusal := parseScopeToolInput(arguments)
	if refusal != "" {
		return errorResult(refusal), "error", nil
	}

	if m.approvalURL == nil {
		return errorResult("Allowing needs the gateway's web UI, which is off."), "error", nil
	}

	named := ""
	if request != nil {
		named = request.Params.RequestState
	}

	// Step 1.
	found, err := m.workspaces.RequestScope(ctx, current.identity, ScopeTarget{Workspace: input.Workspace, Session: input.Session}, input.Servers, named)

	var refused *WorkspaceError

	switch {
	case errors.Is(err, ErrScopeRequestDenied):
		return errorResult(fmt.Sprintf("Adding `%s` was denied (request `%s`).", strings.Join(input.Servers, "`, `"), found.Request)), "error", nil
	case errors.Is(err, ErrScopeRequestExpired):
		return errorResult(m.scopeTimeoutMessage(input.Servers, found.Request)), "error", nil
	case errors.As(err, &refused):
		return errorResult(refused.Message), "error", nil
	case err != nil:
		return errorResult("The gateway couldn't request the change. Try again later."), "failed", nil
	}

	id := found.Request

	// Step 3: a client that can't show the page, or responses for a
	// request the user never saw.
	var responses mcp.InputResponseMap
	if request != nil {
		responses = request.Params.InputResponses
	}

	if request == nil || !supportsURLElicitation(ctx, request) || (responses != nil && found.Inserted) {
		return m.allowResult(accessPending, id), "ok", nil
	}

	// Step 2: ask the client to open the approval page. A request that
	// carries responses never gets input requests back.
	if responses == nil {
		return m.allowInputRequest(input, id), "ok", nil
	}

	if !approvalAccepted(responses, id) {
		return m.allowResult(accessPending, id), "ok", nil
	}

	principal := current.identity.Principal
	if !m.waiters.acquire(principal) {
		return m.allowResult(accessPending, id), "ok", nil
	}
	defer m.waiters.release(principal)

	status, err := m.waitForScopeRequest(ctx, request, current.identity, id)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}

		return errorResult("The gateway couldn't read the request. Try again later."), "failed", nil
	}

	return m.allowResult(status, id), "ok", nil
}

// allowInputRequest asks the client to open the scope request's approval
// page. The elicitation ID and the request state are both the request ID.
func (m *metaTools) allowInputRequest(input scopeToolInput, id string) *mcp.CallToolResult {
	target := fmt.Sprintf("workspace `%s`", input.Workspace)
	if input.Session != "" {
		target = fmt.Sprintf("session `%s`", input.Session)
	}

	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{id: &mcp.ElicitParams{
			Mode:          "url",
			Message:       fmt.Sprintf("Allow `%s` in %s? Open the page to approve or deny the change.", strings.Join(input.Servers, "`, `"), target),
			URL:           m.approvalURL(id),
			ElicitationID: id,
		}},
		RequestState: id,
	}
}

// allowResult is the result for a request in status. A pending request
// isn't a tool error: it exists, and the user can approve it at its page.
func (m *metaTools) allowResult(status, id string) *mcp.CallToolResult {
	url := m.approvalURL(id)
	encoded, _ := json.Marshal(AllowResult{Status: status, Request: id, URL: url})

	var text string

	switch status {
	case accessApproved:
		text = fmt.Sprintf("The change was approved (request `%s`).", id)
	case accessDenied:
		text = fmt.Sprintf("The change was denied (request `%s`).", id)
	case accessExpired:
		text = fmt.Sprintf("Nobody approved the change within %s (request `%s`).", durationWords(m.approvalTimeout), id)
	default:
		text = fmt.Sprintf("Approve this change at %s.", url)
	}

	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}, &mcp.TextContent{Text: string(encoded)}},
		StructuredContent: json.RawMessage(encoded),
	}
}

func (m *metaTools) scopeTimeoutMessage(items []string, id string) string {
	return fmt.Sprintf("Nobody allowed `%s` within %s (request `%s`).", strings.Join(items, "`, `"), durationWords(m.approvalTimeout), id)
}

// waitForScopeRequest waits as a session addition request waits: it
// subscribes before it reads, then re-reads the request on every
// approval:<id> notification, missed event and tick, until it is decided
// or its approvalTimeout passes. It returns the final status.
func (m *metaTools) waitForScopeRequest(ctx context.Context, request *mcp.CallToolRequest, identity auth.Identity, id string) (string, error) {
	events, unsubscribe := m.subscribe()
	defer unsubscribe()

	ticker := time.NewTicker(approvalWaitInterval)
	defer ticker.Stop()

	progress := progressSender{request: request}
	queries := db.New(m.database)

	for {
		row, err := queries.GetAccessRequestForWait(ctx, id)
		if err != nil {
			return "", waitError(ctx, fmt.Errorf("read access request: %w", err))
		}

		status := row.AccessRequest.Status
		progress.send(ctx, fmt.Sprintf("Waiting for request `%s`: it is %s", id, status))

		switch {
		case status != accessPending:
			return status, nil
		case row.Overdue:
			won, err := m.expireAccessRequest(ctx, identity, id)
			if err != nil {
				return "", waitError(ctx, err)
			}

			if won {
				return accessExpired, nil
			}

			// Someone decided first: re-read at once.
			continue
		}

		if err := awaitApprovalWake(ctx, events, ticker.C, id); err != nil {
			return "", err
		}
	}
}

// expireAccessRequest compare-and-sets the overdue request id from pending
// to expired and, only when it wins, writes access.expire and notifies
// approval:<id>.
func (m *metaTools) expireAccessRequest(ctx context.Context, identity auth.Identity, id string) (bool, error) {
	won := false
	kind := ""

	err := pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		expired, err := queries.ExpireDueAccessRequest(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("expire access request: %w", err)
		}

		won = true
		kind = expired.Kind

		if err := audit.NewRecorder(m.auditOutput, queries).Record(ctx, accessEvent(identity, typeAccessExpire, expired)); err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindApproval, id)
	})
	if err == nil && won {
		opsmetrics.ApprovalOutcome(kind, opsmetrics.OutcomeExpired, 1)
	}

	return won, err
}
