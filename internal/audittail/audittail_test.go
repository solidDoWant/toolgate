package audittail_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audittail"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// fakeAudit holds audit rows in key order and answers the tail queries as
// PostgreSQL would, without the commit window.
type fakeAudit struct {
	mu   sync.Mutex
	rows []db.ListAuditTailRow
	next int64
}

func (f *fakeAudit) add(principal string, occurredAt time.Time) db.ListAuditTailRow {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.next++
	row := db.ListAuditTailRow{ID: f.next, Type: "tool.decision", Principal: principal, Details: []byte(`{"decision":"allow"}`)}
	row.OccurredAt.Time = occurredAt.UTC().Truncate(time.Microsecond)
	row.OccurredAt.Valid = true
	f.rows = append(f.rows, row)
	slices.SortFunc(f.rows, func(a, b db.ListAuditTailRow) int {
		if c := a.OccurredAt.Time.Compare(b.OccurredAt.Time); c != 0 {
			return c
		}

		return int(a.ID - b.ID)
	})

	return row
}

func (f *fakeAudit) ListAuditTail(ctx context.Context, arg db.ListAuditTailParams) ([]db.ListAuditTailRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	result := []db.ListAuditTailRow{}

	for _, row := range f.rows {
		after := row.OccurredAt.Time.After(arg.AfterAt.Time) || (row.OccurredAt.Time.Equal(arg.AfterAt.Time) && row.ID > arg.AfterID)
		if after && (arg.Principal == "" || row.Principal == arg.Principal) {
			result = append(result, row)
		}

		if len(result) == int(arg.MaxRows) {
			break
		}
	}

	return result, nil
}

func (f *fakeAudit) ListAuditTailBacklog(ctx context.Context, arg db.ListAuditTailBacklogParams) ([]db.ListAuditTailBacklogRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	result := []db.ListAuditTailBacklogRow{}

	for index := len(f.rows) - 1; index >= 0 && len(result) < int(arg.MaxRows); index-- {
		row := f.rows[index]
		if row.OccurredAt.Time.After(arg.Since.Time) && (arg.Principal == "" || row.Principal == arg.Principal) {
			result = append(result, db.ListAuditTailBacklogRow(row))
		}
	}

	return result, nil
}

const adminGroup = "toolgate-admins"

// identities are what the test middleware puts in the context, by the
// value of the X-Test-Identity header.
var identities = map[string]auth.Identity{
	"alice":         {Principal: "alice", Kind: auth.KindUser, Attestation: auth.AttestationUser, Groups: []string{"eng"}},
	"bob":           {Principal: "bob", Kind: auth.KindUser, Attestation: auth.AttestationUser},
	"admin":         {Principal: "admin", Kind: auth.KindUser, Attestation: auth.AttestationUser, Groups: []string{"eng", adminGroup}},
	"attested-pods": {Principal: "system:serviceaccount:ci:runner", Kind: auth.KindServiceAccount, Attestation: auth.AttestationAttested, Groups: []string{adminGroup}},
	// The admin after leaving the admin group.
	"former-admin": {Principal: "admin", Kind: auth.KindUser, Attestation: auth.AttestationUser, Groups: []string{"eng"}},
}

func init() {
	for index := range audittail.MaxTotal / audittail.MaxPerPrincipal {
		name := fmt.Sprintf("user-%d", index)
		identities[name] = auth.Identity{Principal: name, Kind: auth.KindUser, Attestation: auth.AttestationUser}
	}
}

type harness struct {
	audit   *fakeAudit
	handler *audittail.Handler
	server  *httptest.Server
}

func newHarness(t *testing.T, options audittail.Options) *harness {
	t.Helper()

	h := &harness{audit: &fakeAudit{}}
	options.Queries = h.audit
	options.AdminGroup = adminGroup

	if options.PollInterval == 0 {
		options.PollInterval = 10 * time.Millisecond
	}

	h.handler = audittail.New(options)

	mux := http.NewServeMux()
	h.handler.Register(mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			name := request.Header.Get("X-Test-Identity")
			if name == "" {
				// The real middleware refuses here; the handler must
				// refuse a request without an identity too.
				next.ServeHTTP(writer, request)

				return
			}

			identity := identities[name]

			if expiry := request.Header.Get("X-Test-Expiry"); expiry != "" {
				parsed, err := time.Parse(time.RFC3339Nano, expiry)
				if err != nil {
					http.Error(writer, err.Error(), http.StatusBadRequest)

					return
				}

				identity.Expiry = parsed
			}

			next.ServeHTTP(writer, request.WithContext(auth.WithIdentity(request.Context(), identity)))
		})
	})

	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)

	return h
}

func (h *harness) open(t *testing.T, ctx context.Context, name, after string) *http.Response {
	t.Helper()

	return h.openExpiring(t, ctx, name, after, time.Time{})
}

// openExpiring opens a tail with a token that expires at expiry, or never
// when it is zero.
func (h *harness) openExpiring(t *testing.T, ctx context.Context, name, after string, expiry time.Time) *http.Response {
	t.Helper()

	target := h.server.URL + audittail.Path
	if after != "" {
		target += "?after=" + url.QueryEscape(after)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	require.NoError(t, err)

	if name != "" {
		request.Header.Set("X-Test-Identity", name)
	}

	if !expiry.IsZero() {
		request.Header.Set("X-Test-Expiry", expiry.Format(time.RFC3339Nano))
	}

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

// reader reads the stream's lines in a goroutine, so a test can wait for
// them with a deadline.
type reader struct {
	lines chan string
}

func readLines(body io.Reader) *reader {
	r := &reader{lines: make(chan string, 1024)}

	go func() {
		defer close(r.lines)

		scanner := bufio.NewScanner(body)
		for scanner.Scan() {
			r.lines <- scanner.Text()
		}
	}()

	return r
}

// events waits for count events, skipping keepalive lines.
func (r *reader) events(t *testing.T, count int) []audittail.Event {
	t.Helper()

	var events []audittail.Event

	deadline := time.After(10 * time.Second)

	for len(events) < count {
		select {
		case line, ok := <-r.lines:
			require.True(t, ok, "the stream ended after %d of %d events", len(events), count)

			if line == "" {
				continue
			}

			var event audittail.Event
			require.NoError(t, json.Unmarshal([]byte(line), &event))
			events = append(events, event)
		case <-deadline:
			require.FailNow(t, "timed out", "got %d of %d events", len(events), count)
		}
	}

	return events
}

// ended waits for the stream to end, failing on any further event.
func (r *reader) ended(t *testing.T) {
	t.Helper()

	deadline := time.After(10 * time.Second)

	for {
		select {
		case line, ok := <-r.lines:
			if !ok {
				return
			}

			require.Empty(t, line, "an event after the expected ones")
		case <-deadline:
			require.FailNow(t, "the stream did not end")
		}
	}
}

func principals(events []audittail.Event) []string {
	result := make([]string, 0, len(events))
	for _, event := range events {
		result = append(result, event.Principal)
	}

	return result
}

func TestTailRefusesARequestWithoutIdentity(t *testing.T) {
	h := newHarness(t, audittail.Options{})

	response := h.open(t, t.Context(), "", "")
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
}

func TestTailRefusesAnInvalidAfterKey(t *testing.T) {
	h := newHarness(t, audittail.Options{})

	for _, after := range []string{"x", "12-", "-3", "12-x", "1-2-3"} {
		response := h.open(t, t.Context(), "alice", after)
		assert.Equal(t, http.StatusBadRequest, response.StatusCode, after)
	}
}

func TestTailScope(t *testing.T) {
	tests := []struct {
		identity string
		want     []string
	}{
		{identity: "alice", want: []string{"alice", "alice"}},
		{identity: "bob", want: []string{"bob"}},
		{identity: "admin", want: []string{"alice", "bob", "alice", "system:serviceaccount:ci:runner"}},
		// A service account in the admin group is still not an admin.
		{identity: "attested-pods", want: []string{"system:serviceaccount:ci:runner"}},
	}

	for _, test := range tests {
		t.Run(test.identity, func(t *testing.T) {
			h := newHarness(t, audittail.Options{})
			now := time.Now()

			h.audit.add("alice", now.Add(-time.Hour))
			h.audit.add("bob", now.Add(-50*time.Minute))
			h.audit.add("alice", now.Add(-40*time.Minute))
			h.audit.add("system:serviceaccount:ci:runner", now.Add(-30*time.Minute))

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			response := h.open(t, ctx, test.identity, "")
			require.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, audittail.ContentType, response.Header.Get("Content-Type"))

			lines := readLines(response.Body)
			assert.Equal(t, test.want, principals(lines.events(t, len(test.want))))

			// Rows written later follow, with the same scope.
			h.audit.add("bob", now)
			h.audit.add("alice", now)

			var want []string

			for _, principal := range []string{"bob", "alice"} {
				if test.identity == "admin" || principal == test.identity {
					want = append(want, principal)
				}
			}

			if len(want) > 0 {
				assert.Equal(t, want, principals(lines.events(t, len(want))))
			}
		})
	}
}

func TestTailEventsCarryNoPayload(t *testing.T) {
	h := newHarness(t, audittail.Options{})
	h.audit.add("alice", time.Now().Add(-time.Minute))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	response := h.open(t, ctx, "alice", "")
	line := <-readLines(response.Body).lines

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(line), &fields))

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}

	slices.Sort(keys)
	assert.Equal(t, []string{"attestation", "details", "key", "occurredAt", "principal", "requestId", "server", "session", "tool", "traceId", "type", "workspace"}, keys)
	assert.JSONEq(t, `{"decision":"allow"}`, string(fields["details"]))
}

func TestTailBacklogIsTheLastTwentyOfTheDay(t *testing.T) {
	h := newHarness(t, audittail.Options{})
	now := time.Now()

	h.audit.add("alice", now.Add(-25*time.Hour))

	var want []string

	for index := range 25 {
		row := h.audit.add("alice", now.Add(time.Duration(index-30)*time.Minute))
		if index >= 5 {
			want = append(want, audittail.FormatKey(row.OccurredAt.Time, row.ID))
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	lines := readLines(h.open(t, ctx, "alice", "").Body)

	var keys []string
	for _, event := range lines.events(t, 20) {
		keys = append(keys, event.Key)
	}

	assert.Equal(t, want, keys)

	// The next event is the next row, not a repeat of the backlog.
	next := h.audit.add("alice", now)
	assert.Equal(t, audittail.FormatKey(next.OccurredAt.Time, next.ID), lines.events(t, 1)[0].Key)
}

func TestTailWithoutPastEventsStreamsNewOnes(t *testing.T) {
	h := newHarness(t, audittail.Options{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	lines := readLines(h.open(t, ctx, "alice", "").Body)

	h.audit.add("alice", time.Now())
	assert.Equal(t, []string{"alice"}, principals(lines.events(t, 1)))
}

func TestTailAfterKey(t *testing.T) {
	// No second poll happens during the test: every row must come from
	// the queries that follow a full page at once.
	h := newHarness(t, audittail.Options{PollInterval: time.Hour})
	now := time.Now()

	var keys []string

	// More rows than one query reads, all after the key.
	for index := range audittail.MaxRowsPerQuery + 30 {
		row := h.audit.add("alice", now.Add(-time.Hour).Add(time.Duration(index)*time.Millisecond))
		keys = append(keys, audittail.FormatKey(row.OccurredAt.Time, row.ID))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	lines := readLines(h.open(t, ctx, "alice", keys[9]).Body)

	var got []string
	for _, event := range lines.events(t, len(keys)-10) {
		got = append(got, event.Key)
	}

	assert.Equal(t, keys[10:], got)
}

func TestTailAfterAnOldKeyStartsThirtyOneDaysBack(t *testing.T) {
	h := newHarness(t, audittail.Options{})
	now := time.Now()

	h.audit.add("alice", now.Add(-40*24*time.Hour))
	old := h.audit.add("alice", now.Add(-35*24*time.Hour))
	// After the key, but more than 31 days back.
	h.audit.add("alice", now.Add(-33*24*time.Hour))
	h.audit.add("bob", now.Add(-31*24*time.Hour+time.Hour))
	inRange := h.audit.add("alice", now.Add(-31*24*time.Hour+time.Hour))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	lines := readLines(h.open(t, ctx, "alice", audittail.FormatKey(old.OccurredAt.Time, old.ID)).Body)
	assert.Equal(t, audittail.FormatKey(inRange.OccurredAt.Time, inRange.ID), lines.events(t, 1)[0].Key)
}

func TestTailAfterAKeyOlderThanRetentionStartsAtTheOldestRow(t *testing.T) {
	h := newHarness(t, audittail.Options{})
	now := time.Now()

	// Retention deleted every row before these.
	oldest := h.audit.add("alice", now.Add(-10*24*time.Hour))
	second := h.audit.add("alice", now.Add(-9*24*time.Hour))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	deleted := audittail.FormatKey(now.Add(-20*24*time.Hour), 1)
	lines := readLines(h.open(t, ctx, "alice", deleted).Body)

	events := lines.events(t, 2)
	assert.Equal(t, audittail.FormatKey(oldest.OccurredAt.Time, oldest.ID), events[0].Key)
	assert.Equal(t, audittail.FormatKey(second.OccurredAt.Time, second.ID), events[1].Key)
}

func TestTailEndsAtTokenExpiryAndTheNextTokenDecidesTheScope(t *testing.T) {
	h := newHarness(t, audittail.Options{})
	h.audit.add("alice", time.Now().Add(-time.Minute))
	h.audit.add("bob", time.Now().Add(-time.Minute))

	lines := readLines(h.openExpiring(t, t.Context(), "admin", "", time.Now().Add(300*time.Millisecond)).Body)
	events := lines.events(t, 2)
	lines.ended(t)

	// The admin left the group: the next token has no admin group.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	next := readLines(h.open(t, ctx, "former-admin", events[1].Key).Body)

	h.audit.add("bob", time.Now())
	h.audit.add("admin", time.Now())
	assert.Equal(t, []string{"admin"}, principals(next.events(t, 1)))
}

func TestTailEndsAfterTheMaximumDuration(t *testing.T) {
	h := newHarness(t, audittail.Options{MaxDuration: 300 * time.Millisecond})

	readLines(h.open(t, t.Context(), "alice", "").Body).ended(t)
}

func TestTailEndsOnShutdown(t *testing.T) {
	h := newHarness(t, audittail.Options{})

	response := h.open(t, t.Context(), "alice", "")
	require.Equal(t, http.StatusOK, response.StatusCode)

	h.handler.Shutdown()
	readLines(response.Body).ended(t)
}

func TestTailSendsKeepalives(t *testing.T) {
	h := newHarness(t, audittail.Options{Keepalive: 50 * time.Millisecond})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	lines := readLines(h.open(t, ctx, "alice", "").Body)

	select {
	case line := <-lines.lines:
		assert.Empty(t, line)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no keepalive")
	}
}

func TestTailLimits(t *testing.T) {
	h := newHarness(t, audittail.Options{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	for index := range audittail.MaxPerPrincipal {
		assert.Equal(t, http.StatusOK, h.open(t, ctx, "alice", "").StatusCode, "tail %d", index+1)
	}

	refused := h.open(t, ctx, "alice", "")
	require.Equal(t, http.StatusTooManyRequests, refused.StatusCode)

	body, err := io.ReadAll(refused.Body)
	require.NoError(t, err)
	assert.Equal(t, audittail.TooManyMessage, strings.TrimSpace(string(body)))

	// Another principal still gets one.
	bob, cancelBob := context.WithCancel(ctx)
	assert.Equal(t, http.StatusOK, h.open(t, bob, "bob", "").StatusCode)

	// A closed tail frees its place.
	cancelBob()

	require.Eventually(t, func() bool {
		next, cancelNext := context.WithCancel(ctx)
		defer cancelNext()

		return h.open(t, next, "bob", "").StatusCode == http.StatusOK
	}, 10*time.Second, 10*time.Millisecond)
}

func TestTailReplicaLimit(t *testing.T) {
	h := newHarness(t, audittail.Options{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	for index := range audittail.MaxTotal {
		name := fmt.Sprintf("user-%d", index/audittail.MaxPerPrincipal)
		assert.Equal(t, http.StatusOK, h.open(t, ctx, name, "").StatusCode, "tail %d", index+1)
	}

	require.Equal(t, http.StatusTooManyRequests, h.open(t, ctx, "bob", "").StatusCode)
}

func TestIsAdmin(t *testing.T) {
	tests := []struct {
		name       string
		identity   auth.Identity
		adminGroup string
		want       bool
	}{
		{name: "user in the group", identity: identities["admin"], adminGroup: adminGroup, want: true},
		{name: "user outside the group", identity: identities["alice"], adminGroup: adminGroup},
		{name: "service account in the group", identity: identities["attested-pods"], adminGroup: adminGroup},
		{
			name:       "attested user in the group",
			identity:   auth.Identity{Principal: "admin", Kind: auth.KindUser, Attestation: auth.AttestationAttested, Groups: []string{adminGroup}},
			adminGroup: adminGroup,
			want:       true,
		},
		{
			name:       "SPIFFE workload in the group",
			identity:   auth.Identity{Principal: "spiffe://example.test/admin", Kind: auth.KindSPIFFE, Attestation: auth.AttestationAttested, Groups: []string{adminGroup}},
			adminGroup: adminGroup,
		},
		{name: "user-attested identity of no kind in the group", identity: auth.Identity{Principal: "admin", Attestation: auth.AttestationUser, Groups: []string{adminGroup}}, adminGroup: adminGroup},
		{name: "no admin group", identity: auth.Identity{Principal: "x", Kind: auth.KindUser, Attestation: auth.AttestationUser, Groups: []string{""}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, audittail.IsAdmin(test.identity, test.adminGroup))
		})
	}
}

func TestKeyRoundTrip(t *testing.T) {
	at := time.UnixMicro(1_700_000_000_123_456).UTC()

	key := audittail.FormatKey(at, 42)
	assert.Equal(t, "1700000000123456-42", key)

	parsed, id, ok := audittail.ParseKey(key)
	require.True(t, ok)
	assert.Equal(t, at, parsed)
	assert.Equal(t, int64(42), id)
}
