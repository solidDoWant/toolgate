package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// restoreServers brings back the servers a resumed session kept.
// It decides policy again for each, as Enable
// step 1 does, and runs the insert part of Enable step 4 without waiting. A
// server refused now, no longer offered, or over an instance limit is
// removed from the set with server.disable naming the reason, so resuming
// never widens what the session could do when it ended.
//
// It runs inside the restart's transaction, each server in its own
// savepoint. So a restore that fails or stops part-way rolls back the
// restart too: the session stays ended, and the next request restarts it and
// restores the whole set again.
func (m *metaTools) restoreServers(ctx context.Context, tx pgx.Tx, current requestContext, servers []db.SessionServer) error {
	state, err := m.policyState(ctx, current.identity)
	if err != nil {
		return err
	}

	for _, row := range servers {
		template, err := m.catalog.Offered(row.TemplateName)
		if err != nil || template.Namespace != row.TemplateNs {
			gone := &v1alpha1.MCPServerTemplate{}
			gone.Namespace, gone.Name = row.TemplateNs, row.TemplateName

			if err := m.removeServer(ctx, tx, current, gone, map[string]any{"decision": "refuse", "reason": "not-offered"}); err != nil {
				return err
			}

			continue
		}

		decision := m.decide(ctx, policy.Request{Identity: current.identity, Template: template, State: state})
		if !decision.Allowed {
			if err := m.removeServer(ctx, tx, current, template, refusalDetails(decision)); err != nil {
				return err
			}

			continue
		}

		err = m.restoreInTransaction(ctx, tx, current, template)

		var limitErr *instanceLimitError
		if errors.As(err, &limitErr) {
			if err := m.removeServer(ctx, tx, current, template, map[string]any{
				"decision": "refuse", "reason": "limit", "limit": limitErr.limit,
			}); err != nil {
				return err
			}

			continue
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// restoreInTransaction reuses or inserts the live instance of one restored
// server and writes server.enable for the decision. A server the session no
// longer has, because a disable or another restart came first, is skipped.
// A serialization failure or unique violation is retried once. It runs in a
// savepoint of the restart's transaction outer.
func (m *metaTools) restoreInTransaction(ctx context.Context, outer pgx.Tx, current requestContext, template *v1alpha1.MCPServerTemplate) error {
	lazy, err := m.startsOnCall(ctx, template)
	if err != nil {
		return err
	}

	attempt := func() error {
		return pgx.BeginFunc(ctx, outer, func(tx pgx.Tx) error {
			queries := db.New(tx)

			if err := m.lockSession(ctx, tx, current); err != nil {
				return err
			}

			enabled, err := m.isEnabled(ctx, queries, current.session.ID, template)
			if err != nil || !enabled {
				return err
			}

			// An `onCall` server comes back without an instance.
			var instance db.Instance
			if !lazy {
				instance, err = m.liveOrNewInstance(ctx, queries, current, template)
				if err != nil {
					return err
				}
			}

			details := map[string]any{"decision": "allow", "resume": true}
			if instance.ID != "" {
				details["instance"] = instance.ID
			}

			return m.recordEnable(ctx, tx, current, template, instance, details)
		})
	}

	err = attempt()
	if retryable(err) {
		err = attempt()
	}

	if err != nil && !errors.As(err, new(*instanceLimitError)) {
		return fmt.Errorf("restore `%s`: %w", template.Name, err)
	}

	return err
}
