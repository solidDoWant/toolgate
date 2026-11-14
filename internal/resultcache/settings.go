package resultcache

import (
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// BuiltInServer is the name of the built-in server. Its tools change scope
// state and it has no template, so its calls are never cached.
const BuiltInServer = "toolgate"

// MetaKey is the `_meta` key for the cache: the server's suggestion in its
// tool list, and the age of a hit in a result.
const MetaKey = "toolgate/cache"

// Lifetime bounds and the default lifetime.
const (
	MinTTL     = time.Second
	MaxTTL     = 24 * time.Hour
	DefaultTTL = 5 * time.Minute
)

// SkipReason says why a call of a tool with cache settings doesn't use the
// cache.
type SkipReason string

// Skip reasons.
const (
	// SkipOutdatedInstance: the instance runs an older generation than the
	// template's, so its results must not go under the new generation.
	SkipOutdatedInstance SkipReason = "outdated-instance"
	// SkipNotReadOnly: the live instance doesn't mark the tool readOnlyHint.
	SkipNotReadOnly SkipReason = "not-read-only"
	// SkipArguments: the arguments give no key.
	SkipArguments SkipReason = "arguments"
)

// Decision is whether a call uses the cache.
type Decision struct {
	// Use is true when the call looks up and stores.
	Use bool
	// Skip is set when the tool has cache settings but the call doesn't use
	// them. With neither Use nor Skip, the call has no cache step at all.
	Skip SkipReason
}

// Eligibility decides whether a call uses the cache, before its arguments
// are read. cache is the tool's setting in the template generation that
// applies (the current one when valid, else the last valid one), and
// templateGeneration is that generation. instanceGeneration is the live
// instance's generation, and tool is the tool's entry in the live
// instance's own tool list, or nil when the list doesn't have it. The
// read-only hint comes only from that list, never from the template.
func Eligibility(server string, cache *v1alpha1.ToolCache, templateGeneration, instanceGeneration int64, tool *mcp.Tool) Decision {
	if server == BuiltInServer || cache == nil {
		return Decision{}
	}

	if instanceGeneration != templateGeneration {
		return Decision{Skip: SkipOutdatedInstance}
	}

	if tool == nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		return Decision{Skip: SkipNotReadOnly}
	}

	return Decision{Use: true}
}

// Lifetime returns how long a result is kept: the template's ttl, else the
// server's suggestion in its tool list, `_meta["toolgate/cache"].ttl`, when
// it is a duration of 1s to 24h, else 5 minutes. tool may be nil.
func Lifetime(cache v1alpha1.ToolCache, tool *mcp.Tool) time.Duration {
	if cache.TTL != nil {
		return cache.TTL.Duration
	}

	if suggested, ok := suggestedTTL(tool); ok {
		return suggested
	}

	return DefaultTTL
}

// suggestedTTL reads the server's ttl suggestion. Any other value, type or
// range is ignored.
func suggestedTTL(tool *mcp.Tool) (time.Duration, bool) {
	if tool == nil {
		return 0, false
	}

	settings, ok := tool.Meta[MetaKey].(map[string]any)
	if !ok {
		return 0, false
	}

	text, ok := settings["ttl"].(string)
	if !ok {
		return 0, false
	}

	ttl, err := time.ParseDuration(text)
	if err != nil || ttl < MinTTL || ttl > MaxTTL {
		return 0, false
	}

	return ttl, true
}

// CacheScope returns the cache scope: the setting's scope when set, else the
// template's effective scope.
func CacheScope(cache v1alpha1.ToolCache, effectiveScope string) string {
	if cache.Scope != "" {
		return cache.Scope
	}

	return effectiveScope
}
