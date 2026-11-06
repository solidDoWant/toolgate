package gateway

import "strings"

// synonymGroups hold general computing words that mean nearly the same thing
// to a person describing a task. They apply to any MCP server and hold no
// names of products, servers or tools. A query word in a group also searches
// for the other words of the group.
var synonymGroups = [][]string{
	{"folder", "directory", "dir"},
	{"file", "document", "doc"},
	{"message", "chat", "dm"},
	{"mail", "email", "inbox"},
	{"remove", "delete", "erase"},
	{"show", "list", "get", "read", "view", "display"},
	{"fetch", "download", "retrieve", "get"},
	{"search", "find", "lookup", "query"},
	{"remember", "memory", "note", "store"},
	{"history", "recent", "past", "log"},
	{"create", "add", "new", "make"},
	{"update", "edit", "modify", "change"},
	{"write", "save"},
	{"run", "execute"},
	{"send", "post"},
	{"event", "calendar", "meeting", "schedule"},
	{"everything", "all", "dump"},
	{"cancel", "delete", "remove", "discard"},
	{"reply", "respond", "answer", "comment"},
	{"label", "tag", "category"},
	{"free", "available", "availability", "busy"},
	{"copy", "duplicate", "clone", "fork"},
	{"user", "member", "people", "person", "account"},
	{"room", "channel", "group"},
	{"database", "db", "sql", "table"},
	{"running", "process", "workload", "job"},
	{"know", "knowledge", "remember", "memory", "saved"},
	{"info", "information", "detail", "metadata", "size", "stat"},
	{"draft", "prepare", "compose"},
	{"diff", "change", "difference"},
	{"status", "state", "modified"},
	{"commit", "record", "save"},
	{"relation", "relationship", "link", "connection"},
	{"propose", "request"},
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
