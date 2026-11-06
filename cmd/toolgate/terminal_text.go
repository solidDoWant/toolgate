package main

import (
	"strconv"
	"strings"
	"unicode"
)

// terminalSafe returns text that is safe to print to a terminal. Text with a
// control character (C0, DEL or C1) comes back Go-quoted, so each control
// character shows as a visible escape; other text is returned unchanged.
func terminalSafe(text string) string {
	if strings.IndexFunc(text, unicode.IsControl) < 0 {
		return text
	}

	return strconv.Quote(text)
}
