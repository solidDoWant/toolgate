package gateway

import "encoding/json"

// CanonicalArguments exposes canonicalArguments to tests.
func CanonicalArguments(arguments json.RawMessage) (json.RawMessage, []byte, error) {
	return canonicalArguments(arguments)
}

// DurationWords exposes durationWords to tests.
var DurationWords = durationWords

// SetBeforeApprovalUse makes hook run before each use of an approval, and
// returns a function that removes it.
func SetBeforeApprovalUse(hook func()) func() {
	beforeApprovalUse = hook

	return func() { beforeApprovalUse = nil }
}
