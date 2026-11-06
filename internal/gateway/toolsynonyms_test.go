package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpandWords(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		want  []string
	}{
		{name: "a word with no group is unchanged", words: []string{"zebra"}, want: []string{"zebra"}},
		{name: "a group adds its other words after the query words", words: []string{"folder", "zebra"}, want: []string{"folder", "zebra", "directory", "dir"}},
		{name: "a plural finds its group", words: []string{"folders"}, want: []string{"folders", "directory", "dir"}},
		{name: "duplicates are dropped", words: []string{"folder", "directory"}, want: []string{"folder", "directory", "dir"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, expandWords(test.words))
		})
	}
}
