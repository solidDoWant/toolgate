package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Paths of the Approvals pages (docs/design/phase-2.md §5, "Pages").
const (
	ApprovalsPath = "/ui/approvals"
	// DefaultApprovalTimeout is the approvalTimeout when Options sets none.
	DefaultApprovalTimeout = 10 * time.Minute
)

// Texts the approval page shows.
const (
	approvalExpiredMessage  = "This approval expired."
	approvalApprovedMessage = "This approval was approved."
	approvalDeniedMessage   = "This approval was denied."
	payloadsOffMessage      = "Arguments aren't shown because audit payloads are off."
)

// Audit event types of approval decisions.
const (
	typeApprovalApprove = "approval.approve"
	typeApprovalDeny    = "approval.deny"
)

// ApprovalPath is the page of one approval, which tool call errors link to.
func ApprovalPath(id string) string {
	return ApprovalsPath + "/" + id
}

func (u *UI) registerApprovals() {
	u.Handle("GET "+ApprovalsPath, User, http.HandlerFunc(u.approvalsList))
	u.route("GET "+ApprovalsPath+"/{id}", User, MaxFormBytes, stepUpSignIn, http.HandlerFunc(u.approvalDetail))
	u.Handle("POST "+ApprovalsPath+"/{id}/approve", User, RequireStepUp(
		func(request *http.Request) string { return ApprovalPath(request.PathValue("id")) },
		http.HandlerFunc(u.approve)))
	u.Handle("POST "+ApprovalsPath+"/{id}/deny", User, http.HandlerFunc(u.deny))
}

// stepUpSignIn sends a browser without a UI session straight to a step-up
// sign-in, so a user who opens an approval link signs in once.
func stepUpSignIn(request *http.Request) string {
	return StepUpURL(request.URL.RequestURI())
}

type approvalRow struct {
	ID        string
	Server    string
	Tool      string
	Session   string
	Workspace string
	CreatedAt time.Time
	ExpiresAt time.Time
}

func newApprovalRow(approval db.Approval, session string) approvalRow {
	return approvalRow{
		ID:        approval.ID,
		Server:    approval.TemplateName,
		Tool:      approval.Tool,
		Session:   session,
		Workspace: approval.Workspace,
		CreatedAt: approval.CreatedAt.Time,
		ExpiresAt: approval.ExpiresAt.Time,
	}
}

// approvalsList shows the user's pending approvals.
func (u *UI) approvalsList(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	rows, err := u.queries.ListPendingApprovalsOfPrincipal(ctx, session.Principal)
	if err != nil {
		u.logger.ErrorContext(ctx, "cannot list approvals for the web UI", "error", err)
		http.Error(writer, "The gateway can't read your approvals. Try again later.", http.StatusInternalServerError)

		return
	}

	approvals := make([]approvalRow, 0, len(rows))
	for _, row := range rows {
		approvals = append(approvals, newApprovalRow(row.Approval, row.SessionID))
	}

	u.render(writer, request, "approvals", "Approvals", approvals)
}

type approvalPage struct {
	Approval approvalRow
	// Message is the approval's status when it can't be decided any more.
	Message string
	// StepUpURL is set when approving needs a fresh sign-in first.
	StepUpURL string
	// PayloadsOff is the note shown in place of the arguments.
	PayloadsOff string
}

// approvalDetail shows one of the user's approvals and, while it is
// pending, the forms that decide it.
func (u *UI) approvalDetail(writer http.ResponseWriter, request *http.Request) {
	row, ok := u.ownApproval(writer, request)
	if !ok {
		return
	}

	u.renderApproval(writer, request, http.StatusOK, row)
}

// ownApproval reads the approval named in the path when it belongs to the
// session's principal. Anyone else, admins included, gets 404.
func (u *UI) ownApproval(writer http.ResponseWriter, request *http.Request) (db.GetApprovalOfPrincipalRow, bool) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	row, err := u.queries.GetApprovalOfPrincipal(ctx, db.GetApprovalOfPrincipalParams{
		ID: request.PathValue("id"), Principal: session.Principal,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(writer, request)

		return row, false
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot read an approval for the web UI", "error", err)
		http.Error(writer, "The gateway can't read this approval. Try again later.", http.StatusInternalServerError)

		return row, false
	}

	return row, true
}

func (u *UI) renderApproval(writer http.ResponseWriter, request *http.Request, status int, row db.GetApprovalOfPrincipalRow) {
	session, _ := SessionFrom(request.Context())

	data := approvalPage{
		Approval: newApprovalRow(row.Approval, row.SessionID),
		Message:  approvalStatusMessage(row),
	}

	if data.Message == "" && StepUpNeeded(session) {
		data.StepUpURL = StepUpURL(ApprovalPath(row.Approval.ID))
	}

	if !u.options.AuditPayloads {
		data.PayloadsOff = payloadsOffMessage
	}

	u.renderStatus(writer, request, status, "approval", "Approval", data)
}

// approvalStatusMessage is the approval's real status, or empty while it
// is pending and can still be decided.
func approvalStatusMessage(row db.GetApprovalOfPrincipalRow) string {
	switch row.Approval.Status {
	case "pending":
		if row.PastExpiry {
			return approvalExpiredMessage
		}

		return ""
	case "approved":
		return approvalApprovedMessage
	case "denied":
		return approvalDeniedMessage
	default:
		return approvalExpiredMessage
	}
}

// approve approves a pending approval and starts its usable window. The
// route's RequireStepUp has checked the fresh sign-in.
func (u *UI) approve(writer http.ResponseWriter, request *http.Request) {
	u.decide(writer, request, typeApprovalApprove, func(queries *db.Queries, id string, decidedBy pgtype.Text) (db.Approval, error) {
		return queries.ApproveApproval(request.Context(), db.ApproveApprovalParams{
			ID: id, DecidedBy: decidedBy,
			ApprovalTimeout: pgtype.Interval{Microseconds: u.approvalTimeout().Microseconds(), Valid: true},
		})
	})
}

// deny denies a pending approval. It needs no step-up: denying only
// narrows.
func (u *UI) deny(writer http.ResponseWriter, request *http.Request) {
	u.decide(writer, request, typeApprovalDeny, func(queries *db.Queries, id string, decidedBy pgtype.Text) (db.Approval, error) {
		return queries.DenyApproval(request.Context(), db.DenyApprovalParams{ID: id, DecidedBy: decidedBy})
	})
}

// errApprovalUndecidable is returned by the decision transaction when its
// compare-and-set matched no row.
var errApprovalUndecidable = errors.New("the approval isn't pending")

// decide runs one compare-and-set on a pending approval of the user and,
// only when it wins, writes the event and notifies approval:<id> in the same
// transaction. When it matches nothing, the page shows the real status.
func (u *UI) decide(writer http.ResponseWriter, request *http.Request, eventType string,
	update func(queries *db.Queries, id string, decidedBy pgtype.Text) (db.Approval, error),
) {
	session, _ := SessionFrom(request.Context())
	ctx := request.Context()

	row, ok := u.ownApproval(writer, request)
	if !ok {
		return
	}

	err := u.inTransaction(ctx, func(tx pgx.Tx) error {
		queries := db.New(tx)

		approval, err := update(queries, row.Approval.ID, pgtype.Text{String: session.Principal, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			return errApprovalUndecidable
		}

		if err != nil {
			return err
		}

		err = audit.NewRecorder(u.auditOutput(), queries).Record(ctx, audit.Event{
			Type:        eventType,
			Principal:   session.Principal,
			Attestation: string(auth.AttestationUser),
			Server:      approval.TemplateName,
			Tool:        approval.Tool,
			Details:     map[string]any{"approval": approval.ID},
		})
		if err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindApproval, approval.ID)
	})

	if errors.Is(err, errApprovalUndecidable) {
		row, ok := u.ownApproval(writer, request)
		if ok {
			u.renderApproval(writer, request, http.StatusConflict, row)
		}

		return
	}

	if err != nil {
		u.logger.ErrorContext(ctx, "cannot decide an approval", "error", err)
		http.Error(writer, "The gateway can't save your decision. Try again later.", http.StatusInternalServerError)

		return
	}

	http.Redirect(writer, request, ApprovalPath(row.Approval.ID), http.StatusSeeOther)
}

// beginner starts a transaction; *pgxpool.Pool is one.
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

func (u *UI) inTransaction(ctx context.Context, run func(pgx.Tx) error) error {
	database, ok := u.options.Database.(beginner)
	if !ok {
		return fmt.Errorf("the UI database %T can't start a transaction", u.options.Database)
	}

	return pgx.BeginFunc(ctx, database, run)
}

func (u *UI) approvalTimeout() time.Duration {
	if u.options.ApprovalTimeout > 0 {
		return u.options.ApprovalTimeout
	}

	return DefaultApprovalTimeout
}

func (u *UI) auditOutput() io.Writer {
	if u.options.AuditOutput != nil {
		return u.options.AuditOutput
	}

	return io.Discard
}
