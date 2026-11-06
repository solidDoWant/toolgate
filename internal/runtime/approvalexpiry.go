package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// ApprovalExpirer marks pending approvals and access requests expired once
// their expires_at is more than 1 minute past (docs/design/phase-2.md §2,
// "Approvals"; docs/design/phase-3.md §10, "State"). Only the
// reconciler runs it, on its full passes. The minute leaves a waiting call
// time to expire its own approval first.
type ApprovalExpirer struct {
	Pool *pgxpool.Pool
	// AuditOutput receives the approval.expire and access.expire audit
	// lines. Nil discards them.
	AuditOutput io.Writer
}

// approvalExpiryGrace is how long past its expires_at a pending approval
// stays pending before the leader's pass expires it. An e2e build scales it.
const approvalExpiryGrace = time.Minute

// Pass wraps a full pass so that overdue approvals are expired before it.
func (e *ApprovalExpirer) Pass(pass func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := e.Run(ctx); err != nil {
			return errors.Join(err, pass(ctx))
		}

		return pass(ctx)
	}
}

// Run expires every overdue pending approval and access request. An error
// on one row doesn't keep the others from being expired.
func (e *ApprovalExpirer) Run(ctx context.Context) error {
	grace := testtime.Scale(approvalExpiryGrace)

	overdue, err := db.New(e.Pool).ListOverdueApprovals(ctx, pgtype.Interval{Microseconds: grace.Microseconds(), Valid: true})
	if err != nil {
		return fmt.Errorf("list overdue approvals: %w", err)
	}

	var problems []error

	for _, approval := range overdue {
		if err := e.expire(ctx, approval); err != nil {
			problems = append(problems, fmt.Errorf("expire approval %s: %w", approval.ID, err))
		}
	}

	requests, err := db.New(e.Pool).ListOverdueAccessRequests(ctx, pgtype.Interval{Microseconds: grace.Microseconds(), Valid: true})
	if err != nil {
		problems = append(problems, fmt.Errorf("list overdue access requests: %w", err))
	}

	for _, request := range requests {
		if err := e.expireAccessRequest(ctx, request); err != nil {
			problems = append(problems, fmt.Errorf("expire access request %s: %w", request.ID, err))
		}
	}

	return errors.Join(problems...)
}

// expireAccessRequest compare-and-sets one access request from pending to
// expired and, only when it wins, writes access.expire and notifies
// approval:<id>, as expire does for an approval.
func (e *ApprovalExpirer) expireAccessRequest(ctx context.Context, request db.ListOverdueAccessRequestsRow) error {
	return pgx.BeginFunc(ctx, e.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if _, err := queries.ExpireAccessRequest(ctx, request.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}

			return err
		}

		output := e.AuditOutput
		if output == nil {
			output = io.Discard
		}

		attestation := "user"
		if request.Verified {
			attestation = "attested"
		}

		err := audit.NewRecorder(output, queries).Record(ctx, audit.Event{
			Type:        "access.expire",
			Principal:   request.Principal,
			Workspace:   request.Workspace,
			Session:     request.SessionID.String,
			Attestation: attestation,
			Server:      request.Server.String,
			Details:     map[string]any{"request": request.ID, "kind": request.Kind},
		})
		if err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindApproval, request.ID)
	})
}

// expire compare-and-sets one approval from pending to expired and, only
// when it wins, writes approval.expire and notifies approval:<id>.
func (e *ApprovalExpirer) expire(ctx context.Context, approval db.ListOverdueApprovalsRow) error {
	return pgx.BeginFunc(ctx, e.Pool, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if _, err := queries.ExpireApproval(ctx, approval.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}

			return err
		}

		output := e.AuditOutput
		if output == nil {
			output = io.Discard
		}

		err := audit.NewRecorder(output, queries).Record(ctx, audit.Event{
			Type:        "approval.expire",
			Principal:   approval.Principal,
			Workspace:   approval.Workspace,
			Session:     approval.SessionID,
			Attestation: approval.Attestation,
			Server:      approval.TemplateName,
			Tool:        approval.Tool,
			Details:     map[string]any{"approval": approval.ID},
		})
		if err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindApproval, approval.ID)
	})
}
