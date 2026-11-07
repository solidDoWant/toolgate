package gateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCleanText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		limit int
		want  string
	}{
		{name: "line breaks and controls become spaces", input: "a\nb\x00c\x1b[d\u202e e", limit: 50, want: "a b c [d e"},
		{name: "exactly the limit stays", input: strings.Repeat("x", 10), limit: 10, want: strings.Repeat("x", 10)},
		{name: "one over the limit is cut with an ellipsis", input: strings.Repeat("x", 11), limit: 10, want: strings.Repeat("x", 9) + "…"},
		{name: "runes are not split", input: strings.Repeat("é", 20), limit: 5, want: "éééé…"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, cleanText(test.input, test.limit))
		})
	}
}

func TestInstructionsAreBounded(t *testing.T) {
	var summary catalogSummary

	for range 100 {
		summary.Usable = append(summary.Usable, summaryServer{Name: strings.Repeat("n", 63), Description: strings.Repeat("🙂", 500)})
	}

	text := summary.instructions()
	assert.LessOrEqual(t, len(text), summaryMaxBytes)
	assert.Contains(t, text, "more; use toolgate_find_tools")
}
