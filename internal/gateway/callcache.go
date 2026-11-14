package gateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/resultcache"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/templatescope"
)

const (
	// cacheLookupTimeout bounds a cache lookup. A slow lookup is a miss, so
	// the cache never makes a call much slower than forwarding it.
	cacheLookupTimeout = 2 * time.Second
	// cacheStoreTimeout bounds a cache store. A failed store never fails
	// the call.
	cacheStoreTimeout = 5 * time.Second
)

// cacheCall is what the tool result cache does for one call.
type cacheCall struct {
	// set says the tool has cache settings, so tool.result carries the
	// cache fields.
	set bool
	// use says the call looks up and stores, under key for ttl.
	use  bool
	skip resultcache.SkipReason
	key  resultcache.Key
	ttl  time.Duration
}

// details returns the cache fields of tool.result that are known before
// the lookup: none for a tool without cache settings, and the skip reason
// for a skipped call.
func (c cacheCall) details() map[string]any {
	details := map[string]any{}
	if c.set && !c.use {
		details["cache"] = "skipped"
		details["cache_skip"] = string(c.skip)
	}

	return details
}

// cacheStep decides whether an allowed call uses the cache. template is the
// generation that applies (the last valid one), instance is the live
// instance, and its own tool list is the only source of the read-only hint.
func (m *metaTools) cacheStep(current requestContext, template *v1alpha1.MCPServerTemplate, instance db.Instance, tool string, arguments json.RawMessage) cacheCall {
	if m.resultCache == nil || isBuiltin(template) || instance.ID == "" {
		return cacheCall{}
	}

	override, found := template.Spec.Tools[tool]
	if !found || override.Cache == nil {
		return cacheCall{}
	}

	settings := *override.Cache
	upstream := instanceTool(instance, tool)

	decision := resultcache.Eligibility(template.Name, &settings, template.Generation, instance.Generation, upstream)
	if !decision.Use {
		return cacheCall{set: decision.Skip != "", skip: decision.Skip}
	}

	canonical, ok := resultcache.CanonicalArguments(arguments, settings)
	if !ok {
		return cacheCall{set: true, skip: resultcache.SkipArguments}
	}

	scope := resultcache.CacheScope(settings, templatescope.Effective(template.Spec))

	return cacheCall{
		set: true,
		use: true,
		key: resultcache.Key{
			TemplateUID: string(template.UID),
			Generation:  template.Generation,
			Tool:        tool,
			Scope:       scope,
			ScopeID:     resultcache.ScopeID(scope, current.identity.Principal, current.session.ID),
			Arguments:   canonical,
		},
		ttl: resultcache.Lifetime(settings, upstream),
	}
}

// instanceTool returns the entry of tool in the live instance's own tool
// list, or nil when the list doesn't have it or doesn't decode.
func instanceTool(instance db.Instance, tool string) *mcp.Tool {
	var upstream []*mcp.Tool
	if len(instance.Tools) == 0 || json.Unmarshal(instance.Tools, &upstream) != nil {
		return nil
	}

	for _, entry := range upstream {
		if entry != nil && entry.Name == tool {
			return entry
		}
	}

	return nil
}

// lookUpCached reads the cached result of a call that uses the cache. An
// error or a timeout is a miss; its log names the server and the tool and
// never the arguments.
func (m *metaTools) lookUpCached(ctx context.Context, cache cacheCall, server, tool string) (resultcache.Hit, bool) {
	if !cache.use {
		return resultcache.Hit{}, false
	}

	lookupCtx, cancel := context.WithTimeout(ctx, cacheLookupTimeout)
	defer cancel()

	hit, found, err := m.resultCache.Get(lookupCtx, cache.key)
	if err != nil {
		m.logger.WarnContext(ctx, "tool result cache lookup failed; forwarding the call", "server", server, "tool", tool, "error", err)

		found = false
	}

	result := runtime.CacheMiss
	if found {
		result = runtime.CacheHit
	}

	m.metrics.ObserveCacheLookup(server, result)

	return hit, found
}

// storeCached stores the JSON of an ok result. A failed store is counted
// and logged without the arguments; the call still returns its result.
func (m *metaTools) storeCached(ctx context.Context, cache cacheCall, server, tool string, encoded []byte) {
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheStoreTimeout)
	defer cancel()

	stored, err := m.resultCache.Put(storeCtx, cache.key, encoded, cache.ttl)
	if err != nil {
		m.logger.WarnContext(ctx, "tool result cache store failed", "server", server, "tool", tool, "error", err)
		m.metrics.ObserveCacheStore(runtime.CacheStoreFailed)

		return
	}

	m.metrics.ObserveCacheStore(string(stored))
}
