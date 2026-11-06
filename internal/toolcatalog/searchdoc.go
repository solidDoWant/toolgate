package toolcatalog

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// MaxSearchTextBytes cuts the description part and the input part of a
// search document.
const MaxSearchTextBytes = 4 << 10

// SearchParts are the four weighted parts of a tool's search document, as
// plain text. The database turns each into a weighted tsvector, with weights
// A to D in this order (docs/design/phase-4.md §5).
type SearchParts struct {
	// Name is the exposed name with its word boundaries turned into spaces.
	Name string
	// Description is the curated description, cut at MaxSearchTextBytes.
	Description string
	// Server is the server name and the template description.
	Server string
	// Input is the top-level input property names and their descriptions,
	// cut at MaxSearchTextBytes.
	Input string
}

// SearchPartsOf builds the search document parts of a curated tool of
// template.
func SearchPartsOf(template *v1alpha1.MCPServerTemplate, tool Tool) SearchParts {
	return SearchParts{
		Name:        SplitName(tool.ExposedName),
		Description: cutText(tool.Tool.Description, MaxSearchTextBytes),
		Server:      strings.TrimSpace(template.Name + " " + template.Spec.Description),
		Input:       cutText(inputText(tool.Tool.InputSchema), MaxSearchTextBytes),
	}
}

// SplitName turns the `_`, `-` and `.` characters of name and its camelCase
// boundaries into spaces, so `createIssue` gives "create Issue".
func SplitName(name string) string {
	var out strings.Builder

	var previous rune

	for _, char := range name {
		switch char {
		case '_', '-', '.':
			out.WriteRune(' ')
		default:
			if unicode.IsUpper(char) && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
				out.WriteRune(' ')
			}

			out.WriteRune(char)
		}

		previous = char
	}

	return out.String()
}

// inputText joins the top-level property names of a schema and their
// descriptions, in name order.
func inputText(schema any) string {
	root, isMap := schema.(map[string]any)
	if !isMap {
		return ""
	}

	properties, isMap := root["properties"].(map[string]any)
	if !isMap {
		return ""
	}

	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}

	sort.Strings(names)

	var parts []string

	for _, name := range names {
		parts = append(parts, SplitName(name))

		if property, isMap := properties[name].(map[string]any); isMap {
			if description, isString := property["description"].(string); isString {
				parts = append(parts, description)
			}
		}
	}

	return strings.Join(parts, " ")
}

// cutText cuts text to at most limit bytes without splitting a character.
func cutText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}

	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}

	return text[:cut]
}
