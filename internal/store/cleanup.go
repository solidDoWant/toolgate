package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// DeleteStaleRows deletes web UI sessions that are no longer valid, web UI
// sign-ins older than 10 minutes, and grants, temporary blocks, approvals
// and access requests 7 days after they stopped being live (docs/design/phase-2.md
// §2, "Row cleanup"), and the allow entries and found tools of ended sessions
// (docs/design/phase-4.md §2). The audit log keeps their history. A failed delete
// doesn't keep the others from running.
func DeleteStaleRows(ctx context.Context, queries *db.Queries) error {
	deletes := []struct {
		rows   string
		delete func(context.Context) (int64, error)
	}{
		{"web UI sessions", queries.DeleteInvalidUISessions},
		{"web UI sign-ins", queries.DeleteStaleUILogins},
		{"grants", queries.DeleteStaleGrants},
		{"temporary blocks", queries.DeleteStaleTemporaryBlocks},
		{"approvals", queries.DeleteStaleApprovals},
		{"access requests", queries.DeleteStaleAccessRequests},
		// A 0.4 end transaction already deletes these; this catches sessions
		// a 0.3 leader ended (docs/design/phase-4.md §2, "Cleanup").
		{"allow entries of ended sessions", queries.DeleteEndedSessionAllowEntries},
		{"found tools of ended sessions", queries.DeleteEndedSessionFoundTools},
	}

	var problems []error

	for _, each := range deletes {
		if _, err := each.delete(ctx); err != nil {
			problems = append(problems, fmt.Errorf("delete stale %s: %w", each.rows, err))
		}
	}

	return errors.Join(problems...)
}
