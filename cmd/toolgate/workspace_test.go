package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWorkspaceGateway records the requests and answers from canned values.
type fakeWorkspaceGateway struct {
	mu       sync.Mutex
	current  string
	changes  []map[string]any
	headers  []http.Header
	postCode int
	postBody string
	// statuses are the answers to successive polls; the last one repeats.
	statuses []string
	polls    int
	expires  time.Time
	// listedWorkspace replaces the first workspace name in the list, JSON-encoded.
	listedWorkspace string
	// listBody replaces the whole list body when set.
	listBody string
}

func (f *fakeWorkspaceGateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.headers = append(f.headers, request.Header.Clone())

	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/api/v1/workspaces":
		current := "null"
		if f.current != "" {
			current = `"` + f.current + `"`
		}

		name := `"coder/alice/mail"`
		if f.listedWorkspace != "" {
			name = f.listedWorkspace
		}

		if f.listBody != "" {
			_, _ = io.WriteString(writer, f.listBody)

			return
		}

		_, _ = io.WriteString(writer, `{"current":`+current+`,"defaults":{"verified":"all","unverified":"none"},`+
			`"workspaces":[{"workspace":`+name+`,"servers":["k8s","github"],"updatedAt":"2026-01-02T03:04:05Z"},`+
			`{"workspace":"coder/alice/empty","servers":[],"updatedAt":"2026-01-02T03:04:05Z"}]}`)
	case request.Method == http.MethodPost && request.URL.Path == "/api/v1/workspaces/changes":
		var body map[string]any

		_ = json.NewDecoder(request.Body).Decode(&body)
		f.changes = append(f.changes, body)

		if f.postCode >= 400 {
			http.Error(writer, f.postBody, f.postCode)

			return
		}

		writer.WriteHeader(f.postCode)
		_, _ = io.WriteString(writer, f.postBody)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/v1/access-requests/"):
		index := min(f.polls, len(f.statuses)-1)
		f.polls++

		expires := f.expires
		if expires.IsZero() {
			expires = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		}

		_, _ = io.WriteString(writer, `{"id":"abc","status":"`+f.statuses[index]+`","expiresAt":"`+expires.Format(time.RFC3339)+`"}`)
	default:
		http.NotFound(writer, request)
	}
}

type workspaceRun struct {
	code           int
	stdout, stderr string
	waits          []time.Duration
}

func runWorkspaceWith(t *testing.T, gateway *fakeWorkspaceGateway, now time.Time, args ...string) workspaceRun {
	t.Helper()

	server := httptest.NewServer(gateway)
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer

	var result workspaceRun

	clock := now
	env := workspaceEnv{
		stdout: &stdout,
		stderr: &stderr,
		now:    func() time.Time { return clock },
		wait: func(_ context.Context, d time.Duration) error {
			result.waits = append(result.waits, d)
			clock = clock.Add(d)

			return nil
		},
	}

	access := gatewayAccess{gateway: server.URL, accessToken: "token", attestation: "workload"}
	result.code = runWorkspace(t.Context(), env, args, func(call func(gatewayAccess) error) error { return call(access) })
	result.stdout, result.stderr = stdout.String(), stderr.String()

	return result
}

var testNow = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func TestWorkspaceList(t *testing.T) {
	gateway := &fakeWorkspaceGateway{}
	run := runWorkspaceWith(t, gateway, testNow, "list")

	require.Equal(t, 0, run.code, run.stderr)
	assert.Contains(t, run.stdout, "Default for verified workspaces: all")
	assert.Contains(t, run.stdout, "Default for unverified sessions: none")
	assert.Regexp(t, `(?m)^WORKSPACE\s+SERVERS\s+UPDATED$`, run.stdout)
	assert.Regexp(t, `(?m)^coder/alice/mail\s+k8s,github\s+2026-01-02T03:04:05Z$`, run.stdout)
	assert.Regexp(t, `(?m)^coder/alice/empty\s+-\s+2026-01-02T03:04:05Z$`, run.stdout)
	assert.Equal(t, "Bearer token", gateway.headers[0].Get("Authorization"))
	assert.Equal(t, "workload", gateway.headers[0].Get("X-Toolgate-Attestation"))
}

func TestWorkspaceListUnverifiedAllowList(t *testing.T) {
	gateway := &fakeWorkspaceGateway{listBody: `{"current":null,"defaults":{"verified":"none","unverified":"list",` +
		`"unverifiedServers":["github","k8s"]},"workspaces":[]}`}
	run := runWorkspaceWith(t, gateway, testNow, "list")

	require.Equal(t, 0, run.code, run.stderr)
	assert.Contains(t, run.stdout, "Default for unverified sessions: list\n")
	assert.Contains(t, run.stdout, "Unverified allow list: github,k8s\n")
}

func TestWorkspaceListEntries(t *testing.T) {
	gateway := &fakeWorkspaceGateway{listBody: `{"current":null,"defaults":{"verified":"all","unverified":"none"},"workspaces":[` +
		`{"workspace":"coder/alice/mail","servers":["k8s"],"tools":["github__open_issue"],"denied":["slack"],"updatedAt":"2026-01-02T03:04:05Z"},` +
		`{"workspace":"coder/alice/nolist","servers":null,"tools":[],"denied":["github__delete_repository"],"updatedAt":"2026-01-02T03:04:05Z"}]}`}
	run := runWorkspaceWith(t, gateway, testNow, "list")

	require.Equal(t, 0, run.code, run.stderr)
	assert.Regexp(t, `(?m)^WORKSPACE\s+SERVERS\s+TOOLS\s+DENIED\s+UPDATED$`, run.stdout)
	assert.Regexp(t, `(?m)^coder/alice/mail\s+k8s\s+github__open_issue\s+slack\s+2026-01-02T03:04:05Z$`, run.stdout)
	assert.Regexp(t, `(?m)^coder/alice/nolist\s+\(no list\)\s+-\s+github__delete_repository\s+2026-01-02T03:04:05Z$`, run.stdout)
}

func TestWorkspaceChanges(t *testing.T) {
	tests := []struct {
		name    string
		current string
		args    []string
		want    map[string]any
	}{
		{"allow uses the current workspace", "coder/alice/mail", []string{"allow", "k8s", "github"},
			map[string]any{"action": "allow", "workspace": "coder/alice/mail", "servers": []any{"k8s", "github"}}},
		{"allow with --workspace", "", []string{"allow", "--workspace", "w1", "k8s"},
			map[string]any{"action": "allow", "workspace": "w1", "servers": []any{"k8s"}}},
		{"allow of a tool in a session", "cur", []string{"allow", "--session", "s1", "github__open_issue", "k8s"},
			map[string]any{"action": "allow", "session": "s1", "servers": []any{"github__open_issue", "k8s"}}},
		{"allow of a tool in a workspace", "", []string{"allow", "--workspace", "w1", "github__open_issue"},
			map[string]any{"action": "allow", "workspace": "w1", "servers": []any{"github__open_issue"}}},
		{"flag after servers", "", []string{"deny", "k8s", "--workspace", "w1"},
			map[string]any{"action": "deny", "workspace": "w1", "servers": []any{"k8s"}}},
		{"deny of a tool in a session", "", []string{"deny", "--session", "s1", "github__delete_repository", "k8s"},
			map[string]any{"action": "deny", "session": "s1", "servers": []any{"github__delete_repository", "k8s"}}},
		{"deny uses the current workspace", "cur", []string{"deny", "k8s"},
			map[string]any{"action": "deny", "workspace": "cur", "servers": []any{"k8s"}}},
		{"deny --session", "cur", []string{"deny", "--session", "s1", "k8s"},
			map[string]any{"action": "deny", "session": "s1", "servers": []any{"k8s"}}},
		{"clear", "cur", []string{"clear"}, map[string]any{"action": "clear", "workspace": "cur"}},
		{"defaults", "", []string{"defaults", "--verified", "all", "--unverified", "none"},
			map[string]any{"action": "defaults", "verified": "all", "unverified": "none"}},
		{"defaults one side", "", []string{"defaults", "--unverified", "all"},
			map[string]any{"action": "defaults", "unverified": "all"}},
		{"defaults --unverified list", "", []string{"defaults", "--unverified", "list"},
			map[string]any{"action": "defaults", "unverified": "list"}},
		{"allow --unverified", "cur", []string{"allow", "--unverified", "github", "k8s"},
			map[string]any{"action": "allow", "unverified": true, "servers": []any{"github", "k8s"}}},
		{"deny --unverified", "cur", []string{"deny", "k8s", "--unverified"},
			map[string]any{"action": "deny", "unverified": true, "servers": []any{"k8s"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := &fakeWorkspaceGateway{current: test.current, postCode: http.StatusOK, postBody: `{"applied":true}`}
			run := runWorkspaceWith(t, gateway, testNow, test.args...)

			require.Equal(t, 0, run.code, run.stderr)
			require.Len(t, gateway.changes, 1)
			assert.Equal(t, test.want, gateway.changes[0])
			assert.Empty(t, run.waits)
		})
	}
}

func TestWorkspaceUsageErrors(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"session with workspace", []string{"deny", "--session", "s", "--workspace", "w", "k8s"}, "can't be combined"},
		{"allow session with workspace", []string{"allow", "--session", "s", "--workspace", "w", "k8s"}, "can't be combined"},
		{"no servers", []string{"allow", "--workspace", "w"}, "name at least one server"},
		{"bad default", []string{"defaults", "--verified", "maybe"}, "must be none or all"},
		{"verified default list", []string{"defaults", "--verified", "list"}, "must be none or all"},
		{"bad unverified default", []string{"defaults", "--unverified", "some"}, "must be none, all or list"},
		{"unverified with workspace", []string{"allow", "--unverified", "--workspace", "w", "k8s"}, "can't be combined"},
		{"unverified with session", []string{"deny", "--unverified", "--session", "s", "k8s"}, "can't be combined"},
		{"unknown command", []string{"frobnicate"}, "unknown command"},
		{"clear takes no servers", []string{"clear", "k8s"}, "unexpected argument"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := &fakeWorkspaceGateway{}
			run := runWorkspaceWith(t, gateway, testNow, test.args...)

			assert.Equal(t, 2, run.code)
			assert.Contains(t, run.stderr, test.stderr)
			assert.Empty(t, gateway.changes)
		})
	}
}

func TestWorkspaceNoVerifiedWorkspace(t *testing.T) {
	for _, command := range []string{"allow", "deny", "clear"} {
		t.Run(command, func(t *testing.T) {
			gateway := &fakeWorkspaceGateway{postCode: http.StatusOK, postBody: `{"applied":true}`}
			args := []string{command}

			if command != "clear" {
				args = append(args, "k8s")
			}

			run := runWorkspaceWith(t, gateway, testNow, args...)

			assert.Equal(t, 1, run.code)
			assert.Contains(t, run.stderr, "This machine has no verified workspace. Pass --workspace.")
			assert.Empty(t, gateway.changes)
		})
	}
}

func TestWorkspaceGatewayRefusalsShownAsIs(t *testing.T) {
	for _, test := range []struct {
		code    int
		message string
	}{
		{http.StatusConflict, "Workspace `x` has no list. Add one with `toolgate workspace allow` first."},
		{http.StatusForbidden, "Only a signed-in user can change workspaces."},
	} {
		gateway := &fakeWorkspaceGateway{postCode: test.code, postBody: test.message}
		run := runWorkspaceWith(t, gateway, testNow, "deny", "--workspace", "x", "k8s")

		assert.Equal(t, 1, run.code)
		assert.Equal(t, "toolgate workspace deny: "+test.message+"\n", run.stderr)
	}
}

func TestWorkspaceWaitsForApproval(t *testing.T) {
	tests := []struct {
		name     string
		statuses []string
		expires  time.Time
		code     int
		polls    int
		waits    int
		message  string
	}{
		{"approved after two polls", []string{"pending", "pending", "approved"}, time.Time{}, 0, 3, 2, "Approved"},
		{"denied", []string{"pending", "denied"}, time.Time{}, 1, 2, 1, "denied"},
		{"expired status", []string{"expired"}, time.Time{}, 1, 1, 0, "expired"},
		{"expiry time passes", []string{"pending"}, testNow.Add(6 * time.Second), 1, 4, 3, "expired"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := &fakeWorkspaceGateway{
				postCode: http.StatusAccepted, postBody: `{"request":"abc","url":"https://gw/ui/approvals/abc"}`,
				statuses: test.statuses, expires: test.expires,
			}
			run := runWorkspaceWith(t, gateway, testNow, "allow", "--workspace", "w", "k8s")

			assert.Equal(t, test.code, run.code, run.stderr)
			assert.Equal(t, test.polls, gateway.polls)
			assert.Len(t, run.waits, test.waits)

			for _, wait := range run.waits {
				assert.Equal(t, 2*time.Second, wait)
			}

			assert.Contains(t, run.stdout, "Approve this change at https://gw/ui/approvals/abc. Waiting… (Ctrl-C stops waiting; the request stays open until it expires.)")
			assert.Contains(t, run.stdout+run.stderr, test.message)
		})
	}
}

func TestWorkspaceStopsWaitingOnInterrupt(t *testing.T) {
	gateway := &fakeWorkspaceGateway{
		postCode: http.StatusAccepted, postBody: `{"request":"abc","url":"https://gw/x"}`, statuses: []string{"pending"},
	}
	server := httptest.NewServer(gateway)
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer

	ctx, cancel := context.WithCancel(t.Context())
	env := workspaceEnv{
		stdout: &stdout, stderr: &stderr, now: func() time.Time { return testNow },
		wait: func(ctx context.Context, _ time.Duration) error {
			cancel()

			return ctx.Err()
		},
	}
	access := gatewayAccess{gateway: server.URL, accessToken: "token"}
	code := runWorkspace(ctx, env, []string{"allow", "--workspace", "w", "k8s"}, func(call func(gatewayAccess) error) error { return call(access) })

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "stopped waiting")
}

func TestWorkspaceListEscapesControlCharacters(t *testing.T) {
	encoded, err := json.Marshal("\x1b]0;PWNED\x07\x1b[2K\rFORGED")
	require.NoError(t, err)

	run := runWorkspaceWith(t, &fakeWorkspaceGateway{listedWorkspace: string(encoded)}, testNow, "list")

	require.Equal(t, 0, run.code, run.stderr)
	assert.NotContains(t, run.stdout, "\x1b")
	assert.NotContains(t, run.stdout, "\x07")
	assert.NotContains(t, run.stdout, "\r")
	assert.Contains(t, run.stdout, `\x1b]0;PWNED\a\x1b[2K\rFORGED`)
}
