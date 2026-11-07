package ui

import (
	"bytes"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/principal"
)

var (
	hashedSubject = strings.Repeat("914009fe", 8)
	hexRun        = regexp.MustCompile(`[0-9a-f]{64}`)
	htmlTag       = regexp.MustCompile(`(?s)<[^>]*>`)
)

// visibleText is what a reader sees on a page: no tags, so no title or
// value attribute, and no content of a details element's input.
func visibleText(page string) string {
	return html.UnescapeString(htmlTag.ReplaceAllString(page, " "))
}

func TestNoPageShowsARawSubjectAsVisibleText(t *testing.T) {
	unknown := principal.Of(hashedSubject, nil, "someone-else")
	known := principal.Of(hashedSubject, &principal.Profile{Name: "Fred", Email: "fred@example.com"}, "someone-else")
	now := time.Now()

	row := sessionRow{ID: 1, Principal: hashedSubject, Who: unknown, SessionID: "s", Workspace: "claimed/x", WorkspaceLabel: labelWorkspace("claimed/x"), CreatedAt: now, LastSeenAt: now}
	event := auditEvent{Key: "k", OccurredAt: now, Type: "tool.decision", Principal: hashedSubject, Who: unknown, Workspace: "coder/u/w", WorkspaceLabel: labelWorkspace("coder/u/w")}

	pages := map[string]any{
		"servers": serversPage{
			Instances: []gateway.InstanceEntry{{ID: "i", Owner: hashedSubject, Status: "ready"}},
			Owners:    map[string]principal.Display{hashedSubject: unknown},
		},
		"sessions":    sessionsPage{ShowPrincipal: true, Sessions: []sessionRow{row}},
		"session":     sessionPage{Session: row},
		"audit":       auditPage{Events: []auditEvent{event}},
		"audit_event": event,
		"blocks":      blocksPage{Blocks: []blockRow{{ID: "b", CreatedBy: hashedSubject, Who: unknown, CreatedAt: now, ExpiresAt: now}}},
	}

	for name, data := range pages {
		t.Run(name, func(t *testing.T) {
			var body bytes.Buffer

			require.NoError(t, pageTemplates[name].ExecuteTemplate(&body, "layout", page{Title: name, Section: name, Admin: true, Data: data}))

			assert.NotRegexp(t, hexRun, visibleText(body.String()), "the page shows a raw subject as text")
			assert.Contains(t, body.String(), `title="`+hashedSubject+`"`, "the raw subject stays on hover")
			assert.Contains(t, visibleText(body.String()), "user 914009fe…")
		})
	}

	t.Run("a known person shows by name", func(t *testing.T) {
		var body bytes.Buffer

		data := serversPage{
			Instances: []gateway.InstanceEntry{{ID: "i", Owner: hashedSubject, Status: "ready"}},
			Owners:    map[string]principal.Display{hashedSubject: known},
		}
		require.NoError(t, pageTemplates["servers"].ExecuteTemplate(&body, "layout", page{Title: "s", Section: "servers", Data: data}))
		assert.Contains(t, visibleText(body.String()), "Fred <fred@example.com>")
		assert.NotContains(t, body.String(), "<details")
	})
}
