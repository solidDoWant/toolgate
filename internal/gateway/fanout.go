package gateway

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/solidDoWant/toolgate/internal/store"
)

// fanOutTimeout bounds one re-sync of one MCP session after a notification.
const fanOutTimeout = 10 * time.Second

// fanOut sends list_changed to this replica's MCP sessions when their
// proxied tools change (docs/design/phase-1.md §3, "Fan-out of
// list_changed"). It re-syncs every MCP session on an instance:<id>
// notification and after missed notifications, and the MCP sessions of one
// toolgate session on session:<id>. A sync sends list_changed only when the
// session's list really changed. It runs for the life of the process.
func (e *mcpEndpoint) fanOut(tools *metaTools, events <-chan store.Event) {
	for event := range events {
		switch {
		case event.Missed:
			e.resync(tools, func(requestContext) bool { return true })
		case event.Kind == store.KindInstance:
			tools.dropUnlessReady(event.ID)
			e.resync(tools, func(requestContext) bool { return true })
		case event.Kind == store.KindSession:
			id, err := strconv.ParseInt(event.ID, 10, 64)
			if err != nil {
				continue
			}

			e.resync(tools, func(current requestContext) bool { return current.session.ID == id })
		}
	}
}

// resync brings the proxied tools of every MCP session that matches up to
// date.
func (e *mcpEndpoint) resync(tools *metaTools, matches func(requestContext) bool) {
	e.mu.Lock()

	targets := make([]*sessionTools, 0, len(e.bindings))
	for _, session := range e.bindings {
		targets = append(targets, session.tools)
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
