// Package resultcache decides which tool calls use the tool result cache,
// builds their cache keys, and defines the store that keeps cached results
// for every replica.
package resultcache

import (
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"

	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/templatescope"
)

// keyVersion is the first element of the hashed array. Changing the key's
// layout changes it, so old entries are never read with a new layout.
const keyVersion = "toolgate/result-cache/1"

// Normalize operations of a cache setting.
const (
	OperationTrim          = "trim"
	OperationCollapseSpace = "collapseSpace"
	OperationLowercase     = "lowercase"
)

// Key identifies one cached result: the fixed part, which no setting
// changes, and the canonical arguments.
type Key struct {
	// TemplateUID is the template's metadata.uid.
	TemplateUID string
	// Generation is the template generation whose settings apply.
	Generation int64
	// Tool is the tool's upstream name.
	Tool string
	// Scope is the cache scope: shared, per-user or per-session.
	Scope string
	// ScopeID separates the entries of one scope; see ScopeID.
	ScopeID string
	// Arguments are the canonical arguments; see CanonicalArguments.
	Arguments string
}

// Hash returns the SHA-256 sum that is the entry's primary key. It hashes
// the JSON array of the key's parts, whose strings are escaped, so two
// different keys never give the same input bytes.
func (k Key) Hash() [32]byte {
	encoded, err := json.Marshal([]any{keyVersion, k.TemplateUID, k.Generation, k.Tool, k.Scope, k.ScopeID, k.Arguments})
	if err != nil {
		// Strings and an integer always encode.
		panic("resultcache: encode key: " + err.Error())
	}

	return sha256.Sum256(encoded)
}

// ScopeID returns the scope ID of a cache scope: empty for shared, the
// verified principal for per-user, and the session's row ID in decimal for
// per-session. The row ID belongs to the pair of principal and claimed
// session ID, so the claimed session ID itself never goes in the key.
func ScopeID(scope, principal string, sessionRowID int64) string {
	switch scope {
	case templatescope.PerUser:
		return principal
	case templatescope.PerSession:
		return strconv.FormatInt(sessionRowID, 10)
	default:
		return ""
	}
}

// CanonicalArguments returns the arguments part of the key: the top-level
// members of the arguments object without the ignored ones, with the
// normalized string members changed, in RFC 8785 form with numbers kept as
// written. It returns false when the arguments are not one JSON object, or
// have duplicate member names or invalid UTF-8. Normalizing changes only
// the key; the server always gets the caller's own arguments.
func CanonicalArguments(raw []byte, cache v1alpha1.ToolCache) (string, bool) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}

	// Decoding into a map refuses any value that is not an object, and
	// null leaves the map nil.
	var members map[string]jsontext.Value
	if err := jsonv2.Unmarshal(raw, &members); err != nil || members == nil {
		return "", false
	}

	for _, name := range cache.IgnoreArgs {
		delete(members, name)
	}

	for name, operations := range cache.Normalize {
		value, found := members[name]
		if !found || value.Kind() != '"' {
			continue
		}

		var text string
		if err := jsonv2.Unmarshal(value, &text); err != nil {
			return "", false
		}

		normalized, err := jsonv2.Marshal(normalize(text, operations))
		if err != nil {
			return "", false
		}

		members[name] = normalized
	}

	encoded, err := jsonv2.Marshal(members)
	if err != nil {
		return "", false
	}

	canonical := jsontext.Value(encoded)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false)); err != nil {
		return "", false
	}

	return string(canonical), true
}

// normalize applies operations to text in the fixed order trim,
// collapseSpace, lowercase, whatever the order of the list.
func normalize(text string, operations v1alpha1.NormalizeOperations) string {
	has := func(operation string) bool {
		for _, listed := range operations {
			if listed == operation {
				return true
			}
		}

		return false
	}

	if has(OperationTrim) {
		text = strings.TrimSpace(text)
	}

	if has(OperationCollapseSpace) {
		text = collapseSpace(text)
	}

	if has(OperationLowercase) {
		text = strings.ToLower(text)
	}

	return text
}

// collapseSpace replaces each run of white space with one ASCII space.
func collapseSpace(text string) string {
	var builder strings.Builder

	builder.Grow(len(text))

	inSpace := false

	for _, character := range text {
		if unicode.IsSpace(character) {
			if !inSpace {
				builder.WriteByte(' ')
			}

			inSpace = true

			continue
		}

		inSpace = false

		builder.WriteRune(character)
	}

	return builder.String()
}
