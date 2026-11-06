package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audittail"
)

func TestFormatEventEscapesControlCharacters(t *testing.T) {
	evil := "\x1b]0;PWNED\x07\x1b[2K\rFORGED-mallory__x"

	for _, field := range []string{"principal", "workspace", "session", "server", "tool", "decision"} {
		t.Run(field, func(t *testing.T) {
			event := audittail.Event{
				OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Type: "tool.decision",
				Principal: "p", Workspace: "w", Session: "s", Server: "srv", Tool: "t",
				Details: []byte(`{"decision":"refuse"}`),
			}

			switch field {
			case "principal":
				event.Principal = evil
			case "workspace":
				event.Workspace = evil
			case "session":
				event.Session = evil
			case "server":
				event.Server = evil
			case "tool":
				event.Tool = evil
			case "decision":
				event.Details = []byte(`{"decision":"` + `\u001b]0;PWNED\u0007\u001b[2K\rFORGED-mallory__x` + `"}`)
			}

			line := formatEvent(event)

			for _, char := range line {
				require.False(t, char < 0x20 || (char >= 0x7f && char < 0xa0), "control character %q in %q", char, line)
			}

			assert.Contains(t, line, `\x1b]0;PWNED\a\x1b[2K\rFORGED-mallory__x`)
			assert.True(t, strings.HasPrefix(line, "2026-01-02T03:04:05Z tool.decision "))
		})
	}
}

func TestTerminalSafe(t *testing.T) {
	assert.Equal(t, "plain text/ü", terminalSafe("plain text/ü"))
	assert.Equal(t, `"a\u0085b"`, terminalSafe("a\u0085b"))
	assert.Equal(t, `"a\x7fb"`, terminalSafe("a\x7fb"))
	assert.Equal(t, `"a\nb"`, terminalSafe("a\nb"))
}
