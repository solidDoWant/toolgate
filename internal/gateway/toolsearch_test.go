package gateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestQueryWords(t *testing.T) {
	many := strings.Repeat("a ", maxQueryWords+5)

	tests := []struct {
		name   string
		query  string
		words  []string
		query2 string
	}{
		{name: "lowercases and splits on anything else", query: "Create the GitHub-Issue, now!", words: []string{"create", "the", "github", "issue", "now"}, query2: "create | the | github | issue | now"},
		{name: "digits stay", query: "ipv6 and 404", words: []string{"ipv6", "and", "404"}, query2: "ipv6 | and | 404"},
		{name: "tsquery syntax is dropped", query: "a:* & !b | (c) <-> 'd'", words: []string{"a", "b", "c", "d"}, query2: "a | b | c | d"},
		{name: "a non-Latin script has no words", query: "создать задачу", words: nil, query2: ""},
		{name: "exactly 64 words are kept", query: strings.Repeat("a ", maxQueryWords), words: strings.Fields(strings.Repeat("a ", maxQueryWords))},
		{name: "word 65 is cut", query: many, words: strings.Fields(strings.Repeat("a ", maxQueryWords))},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			words := queryWords(test.query)
			assert.Equal(t, test.words, words)

			if test.query2 != "" {
				assert.Equal(t, test.query2, tsQuery(words))
			}
		})
	}
}
