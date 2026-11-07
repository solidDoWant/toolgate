package ui

import (
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummarizeReadsEachEventType(t *testing.T) {
	tests := []struct {
		name    string
		event   auditEvent
		want    string
		outcome string
	}{
		{"call result", auditEvent{Type: "tool.result", Server: "kubesearch", Tool: "search_releases", Details: `{"outcome":"ok","duration_ms":420}`},
			"Called `kubesearch__search_releases`: ok, 420 ms", "ok"},
		{"call error", auditEvent{Type: "tool.result", Server: "a", Tool: "b", Details: `{"outcome":"error"}`}, "Called `a__b`: error", "error"},
		{"refusal by rule", auditEvent{Type: "tool.decision", Server: "github", Tool: "delete_repository", Details: `{"decision":"refuse","rule":"no-deletes"}`},
			"Refused `github__delete_repository`: blocked by rule *no-deletes*", "refused"},
		{"approval requested", auditEvent{Type: "approval.request", Server: "mail-archiver", Tool: "get_email_raw"},
			"Approval requested: `mail-archiver__get_email_raw`", "pending"},
		{"enable", auditEvent{Type: "server.enable", Server: "kubesearch", Details: `{}`}, "Enabled server `kubesearch`", "ok"},
		{"sign in", auditEvent{Type: "session.start", Attestation: "attested", Workspace: "coder/fred/toolgate-auto"},
			"Signed in (attested, Coder workspace toolgate-auto)", "ok"},
		{"unverified allow", auditEvent{Type: "session.addition", Server: "kubesearch"},
			"Allowed `kubesearch` for unverified sessions (approved)", "approved"},
		{"unknown type", auditEvent{Type: "future.thing"}, "Event future.thing", ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := summarize(test.event)
			assert.Equal(t, test.want, summary.Text())
			assert.Equal(t, test.outcome, summary.Outcome)
		})
	}
}

// gatewayEventTypes lists the types found in the Go source that writes them,
// so a new type without a summary fails here.
func TestEverySourceEventTypeHasASummary(t *testing.T) {
	found := map[string]bool{}
	for _, eventType := range emittedEventTypes(t) {
		found[eventType] = true
	}

	known := map[string]bool{}
	for _, eventType := range knownAuditTypes() {
		known[eventType] = true
	}

	for eventType := range found {
		assert.True(t, known[eventType], "event type %s has no summary", eventType)
		assert.NotEqual(t, "Event "+eventType, summarize(auditEvent{Type: eventType, Details: `{}`}).Text(), eventType)
	}
}

// emittedEventTypes lists the types found in the Go source that writes them.
func emittedEventTypes(t *testing.T) []string {
	t.Helper()

	pattern := regexp.MustCompile(`(?:Type[A-Za-z]* *= *|Type: *|eventType *= *)"([a-z]+\.[a-z]+)"`)
	found := map[string]bool{}

	for _, file := range []string{
		"../audit/audit.go", "../gateway/workspaces.go", "../gateway/grants.go", "../gateway/forward.go",
		"../gateway/approval.go", "../gateway/enable.go", "../gateway/sessions.go", "../runtime/sessionend.go",
		"../runtime/approvalexpiry.go", "../ui/approvals.go",
	} {
		source := readFile(t, file)
		for _, match := range pattern.FindAllStringSubmatch(source, -1) {
			found[match[1]] = true
		}

		for _, match := range regexp.MustCompile(`callEvent\([a-z.]+, "([a-z]+\.[a-z]+)"`).FindAllStringSubmatch(source, -1) {
			found[match[1]] = true
		}
	}

	require.NotEmpty(t, found)

	return slices.Sorted(maps.Keys(found))
}

func TestParseAuditFilterRoundTrips(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	query := url.Values{"range": {"7d"}, "outcome": {"refused"}, "category": {"calls"}, "server": {"github"}, "tool": {"x"}}

	filter, err := parseAuditFilter(query, Session{Principal: "me", Admin: true}, now)
	require.NoError(t, err)

	again, err := parseAuditFilter(filter.values(false), Session{Principal: "me", Admin: true}, now)
	require.NoError(t, err)
	assert.Equal(t, filter, again)

	absolute, err := parseAuditFilter(filter.values(true), Session{Principal: "me", Admin: true}, now)
	require.NoError(t, err)
	assert.Equal(t, filter.From, absolute.From)
	assert.Equal(t, "refused", absolute.Outcome)

	params := filter.params()
	assert.Equal(t, "refused", params.Outcome)
	assert.Equal(t, []string{"tool.decision", "tool.result"}, params.Types)
	assert.Equal(t, now.Add(-7*24*time.Hour), filter.From)
}

func TestParseAuditFilterRejectsUnknownChoices(t *testing.T) {
	now := time.Now()

	for _, query := range []url.Values{{"outcome": {"weird"}}, {"category": {"weird"}}, {"range": {"weird"}}} {
		_, err := parseAuditFilter(query, Session{}, now)
		require.Error(t, err)
	}
}

func TestParseAuditFilterScopesNonAdminToTheirPrincipal(t *testing.T) {
	filter, err := parseAuditFilter(url.Values{"principal": {"other"}}, Session{Principal: "me"}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "me", filter.Principal)
}

func TestEventTypePickerHasEveryEmittedType(t *testing.T) {
	for _, eventType := range emittedEventTypes(t) {
		assert.Contains(t, knownAuditTypes(), eventType)
		assert.NotEqual(t, eventType, auditTypeLabel(eventType), "%s has a readable name", eventType)
	}
}

func TestParseAuditFilterEventTypes(t *testing.T) {
	now := time.Now()

	filter, err := parseAuditFilter(url.Values{"type": {"tool.result", "auth.refuse"}}, Session{}, now)
	require.NoError(t, err)
	assert.Equal(t, []string{"tool.result", "auth.refuse"}, filter.queryTypes())

	again, err := parseAuditFilter(filter.values(false), Session{}, now)
	require.NoError(t, err)
	assert.Equal(t, filter.EventTypes, again.EventTypes)

	narrowed, err := parseAuditFilter(url.Values{"type": {"tool.result", "auth.refuse"}, "category": {"calls"}}, Session{}, now)
	require.NoError(t, err)
	assert.Equal(t, []string{"tool.result"}, narrowed.queryTypes())

	_, err = parseAuditFilter(url.Values{"type": {"nope.nope"}}, Session{}, now)
	require.Error(t, err)
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(data)
}
