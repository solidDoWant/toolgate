package gateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/store/db"
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

func TestFuseRanks(t *testing.T) {
	row := func(name string, matched bool, score float64, model string, vector ...float32) db.SearchToolsWithVectorsRow {
		return db.SearchToolsWithVectorsRow{ExposedName: name, UpstreamName: name, Matched: matched, Score: score, EmbeddingModel: model, Embedding: vector}
	}

	rows := []db.SearchToolsWithVectorsRow{
		row("a_words_only", true, 1, "m"),
		row("b_both", true, 0.5, "m", 1, 0),
		row("c_vector_only", false, 0, "m", 0.9, 0.1),
		row("d_other_model", false, 0, "old", 1, 0),
		row("e_nothing", false, 0, "m", 0, 1),
	}

	names := func(fused []db.SearchToolsRow) []string {
		out := []string{}
		for _, item := range fused {
			out = append(out, item.ExposedName)
		}

		return out
	}

	// b is second in words and first by vector, so it leads; a has words
	// only; c has a vector only; d is ignored (other model); e is third by
	// vector but not by words.
	assert.Equal(t, []string{"b_both", "a_words_only", "c_vector_only", "e_nothing"}, names(fuseRanks(rows, []float32{1, 0}, "m")))

	// A row of an old model gets no vector rank.
	assert.NotContains(t, names(fuseRanks(rows, []float32{1, 0}, "m")), "d_other_model")
}
