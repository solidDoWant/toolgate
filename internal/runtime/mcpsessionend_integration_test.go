//go:build integration

package runtime_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const mcpSessionIdleTimeout = 30 * time.Minute

// mcpSession stores an MCP session of the toolgate session, last used ago
// before the database's clock.
func (f *stopFixture) mcpSession(t *testing.T, session db.Session, ago time.Duration) db.McpSession {
	t.Helper()

	row, err := f.queries.InsertMCPSession(t.Context(), db.InsertMCPSessionParams{
		ID:        fmt.Sprintf("%032x", f.counter.Add(1)),
		SessionPk: session.ID, Principal: session.Principal, SessionID: session.SessionID,
		Workspace: session.Workspace, Attestation: session.Attestation,
		ProtocolVersion: "2025-11-25", InitializeParams: []byte(`{"capabilities":{}}`),
	})
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), "UPDATE mcp_sessions SET last_seen_at = now() - make_interval(secs => $2) WHERE id = $1",
		row.ID, ago.Seconds())
	require.NoError(t, err)

	return row
}

// mcpSessionEnded reports whether the MCP session's row has ended_at set.
func (f *stopFixture) mcpSessionEnded(t *testing.T, id string) bool {
	t.Helper()

	row, err := f.queries.GetMCPSession(t.Context(), id)
	require.NoError(t, err)

	return row.EndedAt.Valid
}

// sessionEnded reports whether the toolgate session has ended_at set.
func (f *stopFixture) sessionEnded(t *testing.T, session db.Session) bool {
	t.Helper()

	row, err := f.queries.LockSession(t.Context(), session.ID)
	require.NoError(t, err)

	return row.EndedAt.Valid
}

// endReasons returns the reason of each session.end event of the session.
func (f *stopFixture) endReasons(t *testing.T, session db.Session) []string {
	t.Helper()

	var reasons []string
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT coalesce(array_agg(details->>'reason' ORDER BY id), '{}') FROM audit_events
		WHERE type = 'session.end' AND principal = $1 AND session = $2`,
		session.Principal, session.SessionID).Scan(&reasons))

	return reasons
}

// listen starts a listener on the fixture's database and returns its
// events once it listens.
func (f *stopFixture) listen(t *testing.T) <-chan store.Event {
	t.Helper()

	listener := store.NewListener(f.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	events, unsubscribe := listener.Subscribe()
	t.Cleanup(unsubscribe)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go func() { _ = listener.Run(ctx) }()

	select {
	case <-listener.Listening():
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not start listening")
	}

	return events
}

// expectEvents waits until every wanted event arrived, failing after 30s.
// Other events are ignored.
func expectEvents(t *testing.T, events <-chan store.Event, want ...store.Event) {
	t.Helper()

	missing := make(map[store.Event]bool, len(want))
	for _, event := range want {
		missing[event] = true
	}

	deadline := time.After(30 * time.Second)

	for len(missing) > 0 {
		select {
		case event := <-events:
			delete(missing, event)
		case <-deadline:
			t.Fatalf("notifications not received within 30s: %v", missing)
		}
	}
}

func (f *stopFixture) endIdleMCPSessions(t *testing.T) {
	t.Helper()

	ender := &runtime.MCPSessionEnder{
		Pool: f.pool, IdleTimeout: mcpSessionIdleTimeout,
		Log: slog.New(slog.NewTextHandler(f.logs, nil)),
	}
	require.NoError(t, ender.Run(t.Context()))
}

func TestMCPSessionEnderEndsIdleConnectorSessionWithItsToolgateSession(t *testing.T) {
	f := newStopFixture(t)
	events := f.listen(t)

	idle := f.session(t, "alice", "mcp.0123456789abcdef0123456789abcdef", "alice@example.com")
	idleMCP := f.mcpSession(t, idle, mcpSessionIdleTimeout+time.Minute)

	recent := f.session(t, "bob", "mcp.fedcba9876543210fedcba9876543210", "bob@example.com")
	recentMCP := f.mcpSession(t, recent, mcpSessionIdleTimeout-time.Minute)

	f.endIdleMCPSessions(t)

	assert.True(t, f.mcpSessionEnded(t, idleMCP.ID), "an idle connector MCP session ends")
	assert.True(t, f.sessionEnded(t, idle), "its toolgate session ends with it")
	assert.Equal(t, []string{runtime.MCPSessionEndReasonIdle}, f.endReasons(t, idle))

	assert.False(t, f.mcpSessionEnded(t, recentMCP.ID), "a connector MCP session used within the timeout stays")
	assert.False(t, f.sessionEnded(t, recent))
	assert.Empty(t, f.endReasons(t, recent))

	expectEvents(t, events,
		store.Event{Kind: store.KindMCPSession, ID: idleMCP.ID},
		store.Event{Kind: store.KindSession, ID: fmt.Sprint(idle.ID)})
}

func TestMCPSessionEnderNeverEndsHelperSession(t *testing.T) {
	f := newStopFixture(t)

	helper := f.session(t, "alice", "helper-session", "alice@example.com")
	helperMCP := f.mcpSession(t, helper, 10*mcpSessionIdleTimeout)

	f.endIdleMCPSessions(t)

	assert.False(t, f.mcpSessionEnded(t, helperMCP.ID), "a helper's MCP session never ends by the MCP idle rule")
	assert.False(t, f.sessionEnded(t, helper))
}

func TestSessionEndEndsItsMCPSessions(t *testing.T) {
	f := newStopFixture(t)
	events := f.listen(t)

	helper := f.session(t, "alice", "helper-session", "alice@example.com")
	first := f.mcpSession(t, helper, 0)
	second := f.mcpSession(t, helper, 0)

	other := f.session(t, "alice", "other-session", "alice@example.com")
	otherMCP := f.mcpSession(t, other, 0)

	f.lastSeen(t, helper, sessionIdleTimeout+time.Minute)
	f.endIdleSessions(t)

	require.True(t, f.sessionEnded(t, helper))
	assert.True(t, f.mcpSessionEnded(t, first.ID), "the toolgate session's end ends its MCP sessions")
	assert.True(t, f.mcpSessionEnded(t, second.ID))
	assert.False(t, f.mcpSessionEnded(t, otherMCP.ID), "another session's MCP session stays")

	expectEvents(t, events,
		store.Event{Kind: store.KindMCPSession, ID: first.ID},
		store.Event{Kind: store.KindMCPSession, ID: second.ID},
		store.Event{Kind: store.KindSession, ID: fmt.Sprint(helper.ID)})

	// A later request restarts the toolgate session, but not the MCP session.
	_, err := f.queries.RestartSession(t.Context(), db.RestartSessionParams{
		ID: helper.ID, Workspace: helper.Workspace, Attestation: helper.Attestation, Kind: helper.Kind,
	})
	require.NoError(t, err)
	assert.True(t, f.mcpSessionEnded(t, first.ID), "a restart doesn't bring back an ended MCP session")
}

func TestEndMCPSession(t *testing.T) {
	tests := []struct {
		name           string
		sessionID      string
		wantSessionEnd bool
	}{
		{name: "connector session ends its toolgate session", sessionID: "mcp.0123456789abcdef0123456789abcdef", wantSessionEnd: true},
		{name: "helper session keeps its toolgate session", sessionID: "helper-session"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newStopFixture(t)
			events := f.listen(t)

			session := f.session(t, "alice", test.sessionID, "alice@example.com")
			row := f.mcpSession(t, session, 0)

			ended, err := runtime.EndMCPSession(t.Context(), f.pool, nil, row.ID)
			require.NoError(t, err)
			assert.True(t, ended)
			assert.True(t, f.mcpSessionEnded(t, row.ID))
			assert.Equal(t, test.wantSessionEnd, f.sessionEnded(t, session))

			want := []store.Event{{Kind: store.KindMCPSession, ID: row.ID}}

			if test.wantSessionEnd {
				assert.Equal(t, []string{runtime.MCPSessionEndReasonDelete}, f.endReasons(t, session))

				want = append(want, store.Event{Kind: store.KindSession, ID: fmt.Sprint(session.ID)})
			} else {
				assert.Empty(t, f.endReasons(t, session))
			}

			expectEvents(t, events, want...)

			ended, err = runtime.EndMCPSession(t.Context(), f.pool, nil, row.ID)
			require.NoError(t, err)
			assert.False(t, ended, "an ended MCP session is left as it is")

			ended, err = runtime.EndMCPSession(t.Context(), f.pool, nil, "00000000000000000000000000000000")
			require.NoError(t, err)
			assert.False(t, ended, "an unknown MCP session is left as it is")
		})
	}
}
