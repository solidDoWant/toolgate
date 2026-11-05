package audit

import "strings"

// sensitiveKeys are detail field names, normalized by normalizeKey, whose
// values are always redacted: secret values, tool arguments and tool
// results.
var sensitiveKeys = map[string]bool{
	"value":       true,
	"secretvalue": true,
	"arguments":   true,
	"args":        true,
	"result":      true,
	"results":     true,
	"payload":     true,
}

// sensitiveFragments redact any detail field whose normalized name contains
// one of them, such as "access_token" or "Authorization".
var sensitiveFragments = []string{
	"token",
	"password",
	"passwd",
	"authorization",
	"cookie",
	"credential",
	"privatekey",
	"apikey",
	"clientsecret",
}

// normalizeKey lowercases key and drops the separators "_", "-" and ".".
func normalizeKey(key string) string {
	return strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
}

func isSensitiveKey(key string) bool {
	normalized := normalizeKey(key)
	if sensitiveKeys[normalized] {
		return true
	}

	for _, fragment := range sensitiveFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}

	return false
}

// redactMap returns a copy of details, decoded from JSON, in which the values of sensitive
// fields, at any depth, are replaced by a placeholder. It never returns nil,
// so empty details are written as {}.
func redactMap(details map[string]any) map[string]any {
	redacted := make(map[string]any, len(details))

	for key, value := range details {
		if isSensitiveKey(key) {
			redacted[key] = redactedPlaceholder

			continue
		}

		redacted[key] = redactValue(value)
	}

	return redacted
}

func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return redactMap(typed)
	case []any:
		items := make([]any, len(typed))
		for index, item := range typed {
			items[index] = redactValue(item)
		}

		return items
	default:
		return value
	}
}
