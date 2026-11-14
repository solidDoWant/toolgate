package resultcache_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/resultcache"
)

func TestArgumentsKey(t *testing.T) {
	tests := []struct {
		name  string
		cache v1alpha1.ToolCache
		first string
		other string
		// same says whether first and other give the same key.
		same bool
	}{
		{name: "member order doesn't matter", first: `{"a":1,"b":"x"}`, other: `{"b":"x","a":1}`, same: true},
		{name: "nested member order doesn't matter", first: `{"a":{"x":1,"y":[{"p":1,"q":2}]}}`, other: `{"a":{"y":[{"q":2,"p":1}],"x":1}}`, same: true},
		{name: "white space doesn't matter", first: `{"a":[1,2],"b":"x"}`, other: " {\n \"b\" : \"x\" ,\t\"a\" : [ 1 , 2 ] } ", same: true},
		{name: "array order matters", first: `{"a":[1,2]}`, other: `{"a":[2,1]}`},
		{name: "string values matter", first: `{"q":"Resistor"}`, other: `{"q":"resistor"}`},
		{name: "no arguments are an empty object", first: ``, other: `{}`, same: true},
		{name: "ignoreArgs removes a member", cache: v1alpha1.ToolCache{IgnoreArgs: []string{"reason"}},
			first: `{"q":"x","reason":"one"}`, other: `{"q":"x","reason":"two"}`, same: true},
		{name: "ignoreArgs removes the member entirely", cache: v1alpha1.ToolCache{IgnoreArgs: []string{"reason"}},
			first: `{"q":"x","reason":"one"}`, other: `{"q":"x"}`, same: true},
		{name: "ignoreArgs keeps other members", cache: v1alpha1.ToolCache{IgnoreArgs: []string{"reason"}},
			first: `{"q":"x","reason":"one"}`, other: `{"q":"y","reason":"one"}`},
		{name: "trim", cache: normalized("trim"), first: `{"q":"  a  b \t"}`, other: `{"q":"a  b"}`, same: true},
		{name: "trim only trims", cache: normalized("trim"), first: `{"q":"a  b"}`, other: `{"q":"a b"}`},
		{name: "collapseSpace", cache: normalized("collapseSpace"), first: "{\"q\":\"a \\t\\n b\\u00a0\\u00a0c\"}", other: `{"q":"a b c"}`, same: true},
		{name: "collapseSpace keeps the ends as one space", cache: normalized("collapseSpace"), first: `{"q":"  a  "}`, other: `{"q":" a "}`, same: true},
		{name: "lowercase", cache: normalized("lowercase"), first: `{"q":"RESISTOR Ω"}`, other: `{"q":"resistor ω"}`, same: true},
		{name: "operations run in the fixed order", cache: normalized("lowercase", "collapseSpace", "trim"),
			first: `{"q":"  10K   Resistor  "}`, other: `{"q":"10k resistor"}`, same: true},
		{name: "normalize applies only to the named member", cache: normalized("lowercase"),
			first: `{"q":"a","other":"A"}`, other: `{"q":"A","other":"a"}`},
		{name: "normalize keeps a non-string value", cache: normalized("trim", "lowercase"),
			first: `{"q":{"A":" X "}}`, other: `{"q":{"A":" X "}}`, same: true},
		{name: "normalize doesn't change a non-string value", cache: normalized("trim", "lowercase"),
			first: `{"q":["A"]}`, other: `{"q":["a"]}`},
		{name: "integers above 2^53 that differ", first: `{"n":9007199254740993}`, other: `{"n":9007199254740992}`},
		{name: "1 and 1.0 differ", first: `{"n":1}`, other: `{"n":1.0}`},
		{name: "a member's escapes don't matter", first: `{"q":"A"}`, other: `{"q":"A"}`, same: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first, ok := resultcache.CanonicalArguments([]byte(test.first), test.cache)
			require.True(t, ok)

			other, ok := resultcache.CanonicalArguments([]byte(test.other), test.cache)
			require.True(t, ok)

			if test.same {
				assert.Equal(t, first, other)
			} else {
				assert.NotEqual(t, first, other)
			}
		})
	}

	t.Run("the canonical form is sorted without white space", func(t *testing.T) {
		canonical, ok := resultcache.CanonicalArguments([]byte(`{ "b" : 1.50 , "a" : { "d" : true , "c" : null } }`), v1alpha1.ToolCache{})
		require.True(t, ok)
		assert.Equal(t, `{"a":{"c":null,"d":true},"b":1.50}`, canonical)
	})

	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "duplicate names give no key", raw: `{"q":"a","q":"b"}`},
		{name: "nested duplicate names give no key", raw: `{"q":{"a":1,"a":2}}`},
		{name: "invalid UTF-8 gives no key", raw: "{\"q\":\"\xff\"}"},
		{name: "invalid UTF-8 in a name gives no key", raw: "{\"\xff\":1}"},
		{name: "an array gives no key", raw: `[{"q":"a"}]`},
		{name: "a string gives no key", raw: `"q"`},
		{name: "null gives no key", raw: `null`},
		{name: "invalid JSON gives no key", raw: `{"q":`},
		{name: "trailing data gives no key", raw: `{"q":1}{"q":2}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, ok := resultcache.CanonicalArguments([]byte(test.raw), v1alpha1.ToolCache{})
			assert.False(t, ok)
		})
	}
}

func normalized(operations ...string) v1alpha1.ToolCache {
	return v1alpha1.ToolCache{Normalize: map[string]v1alpha1.NormalizeOperations{"q": operations}}
}

func TestKeyHash(t *testing.T) {
	base := resultcache.Key{
		TemplateUID: "0b1c", Generation: 3, Tool: "search_parts",
		Scope: "per-user", ScopeID: "alice", Arguments: `{"q":"x"}`,
	}

	changes := map[string]func(*resultcache.Key){
		"template uid": func(key *resultcache.Key) { key.TemplateUID = "0b1d" },
		"generation":   func(key *resultcache.Key) { key.Generation = 4 },
		"tool":         func(key *resultcache.Key) { key.Tool = "search_part" },
		"scope":        func(key *resultcache.Key) { key.Scope = "shared" },
		"scope id":     func(key *resultcache.Key) { key.ScopeID = "bob" },
		"arguments":    func(key *resultcache.Key) { key.Arguments = `{"q":"y"}` },
		"parts shifted between fields": func(key *resultcache.Key) {
			key.Tool, key.Scope = "search_partsper-user", ""
		},
	}

	for name, change := range changes {
		t.Run(name+" changes the hash", func(t *testing.T) {
			changed := base
			change(&changed)
			assert.NotEqual(t, base.Hash(), changed.Hash())
		})
	}

	t.Run("the same key gives the same hash", func(t *testing.T) {
		same := base
		assert.Equal(t, base.Hash(), same.Hash())
	})

	t.Run("per-user and per-session keys with the same text differ", func(t *testing.T) {
		perUser := base
		perUser.Scope, perUser.ScopeID = "per-user", "42"

		perSession := base
		perSession.Scope, perSession.ScopeID = "per-session", "42"

		assert.NotEqual(t, perUser.Hash(), perSession.Hash())
	})
}

func TestScopeID(t *testing.T) {
	assert.Empty(t, resultcache.ScopeID("shared", "alice", 42))
	assert.Equal(t, "alice", resultcache.ScopeID("per-user", "alice", 42))
	assert.Equal(t, "42", resultcache.ScopeID("per-session", "alice", 42))
}
