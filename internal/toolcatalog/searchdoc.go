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
// A, B, C and C in this order (docs/design/phase-4.md §5).
type SearchParts struct {
	// Name is the exposed name and the upstream name with their word
	// boundaries turned into spaces.
	Name string
	// Description is the curated title and description, cut at MaxSearchTextBytes.
	Description string
	// Server is the server name and the template description.
	Server string
	// Input is the top-level input property names, their descriptions and
	// enum values, cut at MaxSearchTextBytes.
	Input string
}

// SearchPartsOf builds the search document parts of a curated tool of
// template.
func SearchPartsOf(template *v1alpha1.MCPServerTemplate, tool Tool) SearchParts {
	return SearchParts{
		Name:        nameText(tool),
		Description: cutText(strings.TrimSpace(tool.Tool.Title+" "+tool.Tool.Description), MaxSearchTextBytes),
		Server:      strings.TrimSpace(template.Name + " " + template.Spec.Description),
		Input:       cutText(inputText(tool.Tool.InputSchema), MaxSearchTextBytes),
	}
}

// nameText is the exposed name and, when it differs, the upstream name, each
// split into words.
func nameText(tool Tool) string {
	text := SplitName(tool.ExposedName)
	if tool.UpstreamName != "" && tool.UpstreamName != tool.ExposedName {
		text += " " + SplitName(tool.UpstreamName)
	}

	return text
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

			if values, isList := property["enum"].([]any); isList {
				for _, value := range values {
					if text, isString := value.(string); isString {
						parts = append(parts, SplitName(text))
					}
				}
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
