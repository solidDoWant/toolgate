//go:build integration

package v1alpha1_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestCRDValidatesCacheSettings(t *testing.T) {
	environment := testutil.StartEnvtest(t)

	duration := func(value time.Duration) *metav1.Duration { return &metav1.Duration{Duration: value} }
	names := func(count int) []string {
		result := make([]string, count)
		for index := range result {
			result[index] = fmt.Sprintf("argument-%d", index)
		}

		return result
	}

	tests := []struct {
		name    string
		cache   v1alpha1.ToolCache
		message string
	}{
		{name: "an empty cache is accepted", cache: v1alpha1.ToolCache{}},
		{name: "every setting at its bound is accepted", cache: v1alpha1.ToolCache{
			TTL:        duration(24 * time.Hour),
			Scope:      "per-session",
			IgnoreArgs: names(64),
			Normalize: map[string]v1alpha1.NormalizeOperations{
				"query": {"trim", "lowercase", "collapseSpace"},
			},
		}},
		{name: "a ttl of 1s is accepted", cache: v1alpha1.ToolCache{TTL: duration(time.Second)}},
		{name: "a ttl under 1s is refused", cache: v1alpha1.ToolCache{TTL: duration(999 * time.Millisecond)},
			message: "ttl must be between 1s and 24h"},
		{name: "a ttl over 24h is refused", cache: v1alpha1.ToolCache{TTL: duration(24*time.Hour + time.Second)},
			message: "ttl must be between 1s and 24h"},
		{name: "an unknown scope is refused", cache: v1alpha1.ToolCache{Scope: "global"},
			message: "scope must be one of per-session, per-user, shared"},
		{name: "65 ignoreArgs are refused", cache: v1alpha1.ToolCache{IgnoreArgs: names(65)},
			message: "spec.tools.search_parts.cache.ignoreArgs: Too many"},
		{name: "an unknown normalize operation is refused", cache: v1alpha1.ToolCache{
			Normalize: map[string]v1alpha1.NormalizeOperations{"query": {"uppercase"}},
		}, message: `Unsupported value: "uppercase"`},
		{name: "four normalize operations are refused", cache: v1alpha1.ToolCache{
			Normalize: map[string]v1alpha1.NormalizeOperations{"query": {"trim", "lowercase", "collapseSpace", "trim"}},
		}, message: "spec.tools.search_parts.cache.normalize.query: Too many"},
		{name: "a normalize name over 128 characters is refused", cache: v1alpha1.ToolCache{
			Normalize: map[string]v1alpha1.NormalizeOperations{strings.Repeat("a", 129): {"trim"}},
		}, message: "each normalize entry names an argument of 1 to 128 characters"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := remoteTemplate(func(spec *v1alpha1.MCPServerTemplateSpec) {
				spec.Tools = map[string]v1alpha1.ToolOverride{"search_parts": {Cache: &test.cache}}
			})

			err := environment.Client.Create(t.Context(), template, client.DryRunAll)
			if test.message == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.message)
		})
	}
}
