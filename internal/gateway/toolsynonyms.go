package gateway

import "strings"

// synonymGroups hold general computing words that mean nearly the same thing
// to a person describing a task. They apply to any MCP server and hold no
// names of products, servers or tools. A query word in a group also searches
// for the other words of the group.
var synonymGroups = [][]string{
	{"folder", "directory"},
	{"file", "document"},
	{"message", "post", "chat"},
	{"mail", "email"},
	{"remove", "delete"},
	{"show", "list", "get", "read"},
	{"fetch", "download"},
	{"search", "find", "lookup"},
	{"remember", "memory", "note"},
	{"history", "recent", "past"},
}

var synonymsOf = func() map[string][]string {
	index := map[string][]string{}

	for _, group := range synonymGroups {
		for _, word := range group {
			for _, other := range group {
				if other != word {
					index[word] = append(index[word], other)
				}
			}
		}
	}

	return index
}()

// maxExpandedWords bounds the query after synonyms are added.
const maxExpandedWords = 256

// expandWords returns words and then the synonyms of each, without
// duplicates, at most maxExpandedWords.
func expandWords(words []string) []string {
	seen := make(map[string]bool, len(words))
	out := make([]string, 0, len(words))

	for _, word := range words {
		if !seen[word] {
			seen[word] = true
			out = append(out, word)
		}
	}

	for _, word := range words {
		for _, synonym := range synonymsOf[baseForm(word)] {
			if !seen[synonym] && len(out) < maxExpandedWords {
				seen[synonym] = true
				out = append(out, synonym)
			}
		}
	}

	return out
}

// baseForm removes a plural or past-tense ending, so "meetings" finds the
// group of "meeting". It is a lookup key only; the database stems the words.
func baseForm(word string) string {
	if _, known := synonymsOf[word]; known {
		return word
	}

	for _, ending := range []string{"es", "s", "ed", "d", "ing"} {
		if base, cut := strings.CutSuffix(word, ending); cut {
			if _, known := synonymsOf[base]; known {
				return base
			}
		}
	}

	return word
}
