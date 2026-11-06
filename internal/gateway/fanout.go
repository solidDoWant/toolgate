package gateway

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// fanOutTimeout bounds one re-sync of one MCP session after a notification.
const fanOutTimeout = 10 * time.Second

// expiryRetry is how long the expiry timer waits before it asks PostgreSQL
// again after the query failed.
const expiryRetry = 10 * time.Second

// catalogChanges are the signals of the informer-fed state policy reads:
// rules and templates. Nil channels never receive.
type catalogChanges struct {
	rules     <-chan struct{}
	templates <-chan struct{}
}

// fanOut sends list_changed to this replica's MCP sessions when their
// proxied tools change (docs/design/phase-1.md §1, "Catalog changes", and
// §3, "Fan-out of list_changed"). It re-syncs every MCP session on an
// instance:<id> notification, after missed notifications, and when a rule or
// template changes, and the MCP sessions of one toolgate session on
// session:<id>, and the MCP sessions of one principal on grant:<id> and
// scope:<id>. A template change re-syncs every session too: a session that
// doesn't enable the template keeps the same list, so it gets nothing. A
// sync sends list_changed only when the session's list really changed. It
// runs for the life of the process.
//
// It also keeps the replica's one expiry timer for the earliest future
// expires_at of live grants and temporary blocks (docs/design/phase-2.md §3,
// "Fan-out of list_changed"). When it fires, every MCP session is re-synced
// and the timer is set again. It is also reset after every grant: or block:
// notification and after missed notifications.
func (e *mcpEndpoint) fanOut(tools *metaTools, events <-chan store.Event, changed catalogChanges) {
	everyone := func(requestContext) bool { return true }

	expiry := time.NewTimer(time.Hour)
	expiry.Stop()

	resetExpiry := func() {
		expiry.Stop()

		if delay, ok := e.nextPolicyExpiry(tools); ok {
			expiry.Reset(delay)
		}
	}

	resetExpiry()

	for {
		select {
		case <-changed.rules:
			e.resync(tools, everyone)
		case <-changed.templates:
			e.resync(tools, everyone)
		case <-expiry.C:
			e.resync(tools, everyone)
			resetExpiry()
		case event, open := <-events:
			if !open {
				events = nil

				continue
			}

			e.handleEvent(tools, event)

			if event.Missed || event.Kind == store.KindGrant || event.Kind == store.KindBlock {
				resetExpiry()
			}
		}
	}
}

// nextPolicyExpiry returns the delay until the earliest future expires_at
// of the live grants and temporary blocks, computed by PostgreSQL, and
// false when there is none.
func (e *mcpEndpoint) nextPolicyExpiry(tools *metaTools) (time.Duration, bool) {
	if tools.database == nil {
		return 0, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), fanOutTimeout)
	defer cancel()

	delay, err := db.New(tools.database).NextPolicyExpiry(ctx)
	if err != nil {
		e.logger.Error("reading the next grant or block expiry failed", slog.String("error", err.Error()))

		return expiryRetry, true
	}

	if delay < 0 {
		return 0, false
	}

	return time.Duration(delay) * time.Millisecond, true
}

// handleEvent re-syncs the MCP sessions a PostgreSQL notification affects.
func (e *mcpEndpoint) handleEvent(tools *metaTools, event store.Event) {
	switch {
	case event.Missed:
		e.resync(tools, func(requestContext) bool { return true })
	case event.Kind == store.KindInstance:
		tools.dropUnlessReady(event.ID)
		e.resync(tools, func(requestContext) bool { return true })
	case event.Kind == store.KindBlock:
		e.resync(tools, func(requestContext) bool { return true })
	case event.Kind == store.KindGrant:
		e.resyncGrantPrincipal(tools, event.ID)
	case event.Kind == store.KindScope:
		e.resyncScopePrincipal(tools, event.ID)
	case event.Kind == store.KindSession:
		id, err := strconv.ParseInt(event.ID, 10, 64)
		if err != nil {
			return
		}

		e.resync(tools, func(current requestContext) bool { return current.session.ID == id })
	}
}

// resyncGrantPrincipal re-syncs the MCP sessions of the grant's principal.
func (e *mcpEndpoint) resyncGrantPrincipal(tools *metaTools, id string) {
	if tools.database == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), fanOutTimeout)
	grant, err := db.New(tools.database).GetGrant(ctx, id)

	cancel()

	if err != nil {
		// Without the row the principal is unknown, so every session is
		// re-synced; a sync sends list_changed only when a list changed.
		e.logger.Error("reading a changed grant failed", slog.String("error", err.Error()))
		e.resync(tools, func(requestContext) bool { return true })

		return
	}

	e.resync(tools, func(current requestContext) bool { return current.identity.Principal == grant.Principal })
}

// resyncScopePrincipal re-syncs the MCP sessions of the principal whose
// user_scopes row changed (docs/design/phase-3.md §10, "The 5 s bound").
func (e *mcpEndpoint) resyncScopePrincipal(tools *metaTools, id string) {
	if tools.database == nil {
		return
	}

	everyone := func(requestContext) bool { return true }

	scopeID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		e.resync(tools, everyone)

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), fanOutTimeout)
	principal, err := db.New(tools.database).GetUserScopePrincipal(ctx, scopeID)

	cancel()

	if err != nil {
		// Without the row the principal is unknown, so every session is
		// re-synced; a sync sends list_changed only when a list changed.
		e.logger.Error("reading a changed user scope failed", slog.String("error", err.Error()))
		e.resync(tools, everyone)

		return
	}

	e.resync(tools, func(current requestContext) bool { return current.identity.Principal == principal })
}

// resync brings the proxied tools of every MCP session that matches up to
// date.
func (e *mcpEndpoint) resync(tools *metaTools, matches func(requestContext) bool) {
	e.mu.Lock()

	targets := make([]*sessionTools, 0, len(e.bindings)+len(e.stateless))
	for _, session := range e.bindings {
		targets = append(targets, session.tools)
	}

	for _, binding := range e.stateless {
		targets = append(targets, binding.tools)
	}

	e.mu.Unlock()

	for _, target := range targets {
		current := target.currentRequest()
		if !matches(current) {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), fanOutTimeout)

		wanted, _, err := tools.proxiedTools(ctx, current)
		if err == nil {
			err = target.apply(wanted)
		}

		cancel()

		if err != nil {
			e.logger.Error("tool list sync failed", slog.String("error", err.Error()))
		}
	}
}
