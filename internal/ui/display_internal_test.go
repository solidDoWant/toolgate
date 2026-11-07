package ui

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

	for _, test := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"seconds", now.Add(-59 * time.Second), "just now"},
		{"one minute", now.Add(-time.Minute), "1 min ago"},
		{"minutes", now.Add(-2*time.Minute - 30*time.Second), "2 min ago"},
		{"last minute of the hour", now.Add(-59 * time.Minute), "59 min ago"},
		{"one hour", now.Add(-time.Hour), "1 h ago"},
		{"hours", now.Add(-23 * time.Hour), "23 h ago"},
		{"one day", now.Add(-24 * time.Hour), "1 d ago"},
		{"days", now.Add(-72 * time.Hour), "3 d ago"},
		{"the future", now.Add(time.Minute), "just now"},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := relativeTime(now, test.at.In(time.FixedZone("x", 3600)))
			assert.Equal(t, test.want, view.Text)
			assert.Equal(t, test.at.UTC().Format("2006-01-02 15:04:05")+" UTC", view.UTC)
		})
	}

	assert.Equal(t, timeView{}, relativeTime(now, time.Time{}))
}

func TestOutcomeBadge(t *testing.T) {
	for outcome, want := range map[string]outcomeView{
		"ok":       {"ok", "badge-ok"},
		"allow":    {"ok", "badge-ok"},
		"approved": {"approved", "badge-ok"},
		"pending":  {"pending", "badge-warn"},
		"refused":  {"refused", "badge-bad"},
		"error":    {"error", "badge-bad"},
		"denied":   {"denied", "badge-bad"},
		"deny":     {"denied", "badge-bad"},
		" Error ":  {"error", "badge-bad"},
		"other":    {"other", "badge-neutral"},
	} {
		assert.Equal(t, want, outcomeBadge(outcome), outcome)
	}
}

func TestSessionState(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	window := 24 * time.Hour

	at := func(ago time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: now.Add(-ago), Valid: true}
	}

	for _, test := range []struct {
		name     string
		row      sessionRowInput
		want     string
		wantTime string
	}{
		{"request just inside five minutes", sessionRowInput{seen: 5*time.Minute - time.Second}, "active", ""},
		{"request exactly five minutes ago", sessionRowInput{seen: 5 * time.Minute}, "idle", ""},
		{"ended with time left", sessionRowInput{seen: time.Hour, ended: time.Hour}, "resumable", "2026-11-08 11:00:00 UTC"},
		{"ended at the end of the window", sessionRowInput{seen: 25 * time.Hour, ended: 24 * time.Hour}, "ended", ""},
		{"ended just inside the window", sessionRowInput{seen: 25 * time.Hour, ended: 24*time.Hour - time.Second}, "resumable", "2026-11-07 12:00:01 UTC"},
		{"connector never resumes", sessionRowInput{id: "mcp.abc", seen: time.Hour, ended: time.Hour}, "ended", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := dbSession(test.row.id, at(test.row.seen), test.row.ended, at)
			state := stateOf(now, row, window)
			assert.Equal(t, test.want, state.Name)
			assert.Equal(t, test.wantTime, state.Until.UTC)
		})
	}
}

type sessionRowInput struct {
	id    string
	seen  time.Duration
	ended time.Duration
}

func dbSession(id string, seen pgtype.Timestamptz, ended time.Duration, at func(time.Duration) pgtype.Timestamptz) db.Session {
	if id == "" {
		id = "abcd1234"
	}

	row := db.Session{SessionID: id, LastSeenAt: seen}
	if ended > 0 {
		row.EndedAt = at(ended)
	}

	return row
}
