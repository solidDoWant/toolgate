package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// recordingResolvers answers each resolver with a fixed expiry and records
// the requests.
type recordingResolvers struct {
	expiries map[string]time.Time
	requests []runtime.ResolveRequest
}

func (r *recordingResolvers) Resolve(_ context.Context, request runtime.ResolveRequest) (runtime.Resolved, error) {
	r.requests = append(r.requests, request)

	return runtime.Resolved{Value: "value-of-" + request.Resolver, ExpiresAt: r.expiries[request.Resolver]}, nil
}

func TestRenderCallsEachResolverOnce(t *testing.T) {
	earliest := time.Date(2030, 1, 1, 0, 5, 0, 0, time.UTC)

	for _, scope := range []string{"per-user", "per-session"} {
		t.Run(scope, func(t *testing.T) {
			resolvers := &recordingResolvers{expiries: map[string]time.Time{
				"first":  earliest.Add(time.Hour),
				"second": earliest,
			}}

			input := newInput(t, scope, `apiVersion: v1
kind: Secret
metadata: {name: creds}
stringData:
  a: "${resolver:first}"
  b: "${resolver:second}"
  c: "${resolver:first}-again"
`)
			input.Identity.PrincipalGroups = []string{"dev"}
			input.Resolvers = resolvers

			result, err := runtime.Render(t.Context(), input)
			require.NoError(t, err)

			assert.Equal(t, "value-of-first-again", nestedString(t, result.Objects[0], "stringData", "c"))
			assert.True(t, earliest.Equal(result.ResolvedExpiresAt), "earliest expiry, got %s", result.ResolvedExpiresAt)
			require.Len(t, resolvers.requests, 2)

			for _, request := range resolvers.requests {
				assert.Equal(t, runtime.ResolveTemplate{Namespace: "toolgate-catalog", Name: "echo"}, request.Template)
				assert.Equal(t, input.Instance.ID, request.Instance)
				assert.Equal(t, runtime.ResolvePrincipal{ID: "user-sub-1", Email: "alice@example.com", Name: "Alice", Groups: []string{"dev"}}, request.Principal)

				if scope == "per-session" {
					assert.Equal(t, "claimed/laptop", request.Workspace)
					assert.Equal(t, "session-ABC", request.Session)
				} else {
					assert.Empty(t, request.Workspace)
					assert.Empty(t, request.Session)
				}
			}
		})
	}
}

func TestRenderWithoutResolvers(t *testing.T) {
	input := newInput(t, "per-user", `apiVersion: v1
kind: Secret
metadata: {name: creds}
stringData:
  a: "${resolver:first}"
`)
	input.Resolvers = nil

	_, err := runtime.Render(t.Context(), input)
	require.ErrorContains(t, err, "${resolver:first} could not be resolved")
}
