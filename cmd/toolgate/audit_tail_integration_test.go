//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audittail"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const tailAdminGroup = "toolgate-admins"

// runningTail is a `toolgate audit tail --json` running in the background.
type runningTail struct {
	stdout *lockedBuffer
	stderr *lockedBuffer
	cancel context.CancelFunc
	done   chan error
}

func startTail(t *testing.T, after string) *runningTail {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	tail := &runningTail{stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, cancel: cancel, done: make(chan error, 1)}

	runner := &tailer{
		after: after, raw: true, stdout: tail.stdout, stderr: tail.stderr,
		minBackoff: 50 * time.Millisecond, maxBackoff: 200 * time.Millisecond,
	}

	go func() { tail.done <- runner.run(ctx) }()

	t.Cleanup(tail.stop)

	return tail
}

func (r *runningTail) stop() {
	r.cancel()
	err := <-r.done

	r.done <- err
}

// events returns the events printed so far.
func (r *runningTail) events(t *testing.T) []audittail.Event {
	t.Helper()

	var events []audittail.Event

	for line := range strings.SplitSeq(r.stdout.String(), "\n") {
		if line == "" {
			continue
		}

		var event audittail.Event
		require.NoError(t, json.Unmarshal([]byte(line), &event), line)
		events = append(events, event)
	}

	return events
}

// waitFor waits until an event of type eventType has been printed.
func (r *runningTail) waitFor(t *testing.T, eventType string) {
	t.Helper()

	require.Eventually(t, func() bool {
		return strings.Contains(r.stdout.String(), `"type":"`+eventType+`"`)
	}, time.Minute, 50*time.Millisecond, "no %s event; stderr: %s", eventType, r.stderr.String())
}

func insertEvent(t *testing.T, queries *db.Queries, eventType, principal string) {
	t.Helper()

	require.NoError(t, queries.InsertAuditEvent(t.Context(), db.InsertAuditEventParams{
		Type: eventType, Principal: principal, Details: []byte(`{"decision":"allow"}`),
		Payload: []byte(`{"arguments":"payload-never-streamed"}`),
	}))
}

func TestAuditTail(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("TOOLGATE_ADMIN_GROUP", tailAdminGroup)

	served := startServeWithSecrets(t)
	done := served.done

	defer func() { stopServe(t, served.stderr, done) }()

	served.provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"}})
	served.provider.AddUser(testutil.OIDCUser{Subject: "root", Email: "root@example.com", EmailVerified: true, Groups: []string{"eng", tailAdminGroup}})

	conn, err := pgx.Connect(t.Context(), served.databaseURL)
	require.NoError(t, err)

	defer func() { _ = conn.Close(t.Context()) }()

	queries := db.New(conn)

	_, err = conn.Exec(t.Context(), `INSERT INTO audit_events (occurred_at, type, principal, payload) VALUES
		(now() - interval '2 minutes', 'past.alice', 'alice', '{"secret":"payload-never-streamed"}'),
		(now() - interval '1 minute', 'past.bob', 'bob', NULL)`)
	require.NoError(t, err)

	t.Run("an unauthenticated request is refused", func(t *testing.T) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, served.url+audittail.Path, nil)
		require.NoError(t, err)

		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)

		defer func() { _ = response.Body.Close() }()

		assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	})

	t.Run("toolgate audit tail streams the user's own events, and all events for an admin", func(t *testing.T) {
		code, _, signInStderr := signInApproving(t, served.provider, "alice", "--gateway", served.url)
		require.Equal(t, 0, code, "stderr: %s", signInStderr)

		tail := startTail(t, "")
		tail.waitFor(t, "past.alice")

		insertEvent(t, queries, "new.bob", "bob")
		insertEvent(t, queries, "new.alice", "alice")
		tail.waitFor(t, "new.alice")
		tail.stop()

		for _, event := range tail.events(t) {
			assert.Equal(t, "alice", event.Principal, "a non-admin sees only their own events")
		}

		assert.NotContains(t, tail.stdout.String(), "payload-never-streamed")

		code, _, signInStderr = signInApproving(t, served.provider, "root", "--gateway", served.url)
		require.Equal(t, 0, code, "stderr: %s", signInStderr)

		admin := startTail(t, "")
		admin.waitFor(t, "new.alice")
		admin.stop()

		var types []string

		for _, event := range admin.events(t) {
			if strings.HasPrefix(event.Type, "past.") || strings.HasPrefix(event.Type, "new.") {
				types = append(types, event.Type)
			}
		}

		assert.Equal(t, []string{"past.alice", "past.bob", "new.bob", "new.alice"}, types)
	})

	t.Run("a row that commits after a newer one is streamed in key order", func(t *testing.T) {
		code, _, signInStderr := signInApproving(t, served.provider, "alice", "--gateway", served.url)
		require.Equal(t, 0, code, "stderr: %s", signInStderr)

		tail := startTail(t, "")
		tail.waitFor(t, "new.alice")

		// A transaction that started before the newest streamed row.
		late, err := conn.Begin(t.Context())
		require.NoError(t, err)

		defer func() { _ = late.Rollback(t.Context()) }()

		_, err = late.Exec(t.Context(), "SELECT 1")
		require.NoError(t, err)

		other, err := pgx.Connect(t.Context(), served.databaseURL)
		require.NoError(t, err)

		defer func() { _ = other.Close(t.Context()) }()

		insertEvent(t, db.New(other), "order.early", "alice")
		tail.waitFor(t, "order.early")

		// The old transaction inserts a row, a newer row commits first,
		// then the old transaction commits.
		insertEvent(t, db.New(late), "order.first", "alice")
		insertEvent(t, db.New(other), "order.second", "alice")
		require.NoError(t, late.Commit(t.Context()))

		tail.waitFor(t, "order.second")
		tail.stop()

		var types []string

		for _, event := range tail.events(t) {
			if strings.HasPrefix(event.Type, "order.") {
				types = append(types, event.Type)
			}
		}

		assert.Equal(t, []string{"order.early", "order.first", "order.second"}, types)
	})

	t.Run("the CLI reconnects after a gateway restart with no duplicates", func(t *testing.T) {
		code, _, signInStderr := signInApproving(t, served.provider, "alice", "--gateway", served.url)
		require.Equal(t, 0, code, "stderr: %s", signInStderr)

		tail := startTail(t, "")
		tail.waitFor(t, "order.second")

		insertEvent(t, queries, "restart.before", "alice")
		tail.waitFor(t, "restart.before")

		// Shutdown ends the open tail, so serve drains and exits 0.
		stopServe(t, served.stderr, done)

		insertEvent(t, queries, "restart.during", "alice")

		t.Setenv("TOOLGATE_LISTEN_ADDRESS", strings.TrimPrefix(served.url, "http://"))

		stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
		done = make(chan int, 1)

		go func() { done <- run([]string{"serve"}, stdout, stderr) }()

		served.stderr = stderr

		insertEvent(t, queries, "restart.after", "alice")
		tail.waitFor(t, "restart.after")
		tail.stop()

		seen := map[string]int{}

		var types []string

		for _, event := range tail.events(t) {
			seen[event.Key]++
			types = append(types, event.Type)
		}

		for key, count := range seen {
			assert.Equal(t, 1, count, "event %s printed more than once", key)
		}

		assert.Equal(t, []string{"restart.before", "restart.during", "restart.after"}, types[len(types)-3:])
	})
}
