//go:build integration

package runtime_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

func TestStarterConnectsToRemoteServersWithTheUsersHeaders(t *testing.T) {
	const aliceKey = "ma_alice_integration_key"

	tests := []struct {
		name string
		// key is alice's stored secret; empty means none is set.
		key       string
		stateless bool
		// refusal is the status the upstream answers a wrong key with.
		refusal    int
		wantStatus string
		wantReason string
		wantTools  bool
	}{
		{name: "stateful upstream with the secret set", key: aliceKey, refusal: http.StatusUnauthorized, wantStatus: "ready", wantTools: true},
		{name: "stateless upstream with the secret set", key: aliceKey, stateless: true, refusal: http.StatusUnauthorized, wantStatus: "ready", wantTools: true},
		{
			name: "secret missing", refusal: http.StatusUnauthorized, wantStatus: "failed",
			wantReason: "Secret `archive-key` isn't set. Run `toolgate secret set archive-key`.",
		},
		{
			name: "upstream answers 401", key: "ma_wrong", refusal: http.StatusUnauthorized, wantStatus: "failed",
			wantReason: "The remote server refused your credential (401 or 403). Check the secret and set it again with `toolgate secret set archive-key`.",
		},
		{
			name: "upstream answers 403", key: "ma_wrong", refusal: http.StatusForbidden, wantStatus: "failed",
			wantReason: "The remote server refused your credential (401 or 403). Check the secret and set it again with `toolgate secret set archive-key`.",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := newBearerServer(t, aliceKey, test.stateless, test.refusal)
			template := remoteTemplate(upstream.URL)

			queries, startStore := catalogPool(t)
			metrics := runtime.NewMetrics()
			startStore.Metrics = metrics

			_, err := queries.InsertInstance(t.Context(), db.InsertInstanceParams{
				ID: instanceID, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: templateUID,
				Generation: template.Generation, Scope: "per-user", ScopeKey: "user:alice", OwnerPrincipal: "alice",
			})
			require.NoError(t, err)

			values := map[string]map[string]string{}
			if test.key != "" {
				values["alice"] = map[string]string{"archive-key": test.key}
			}

			var logs bytes.Buffer

			starter := &runtime.Starter{
				Store: startStore,
				Template: func(types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
					return template.DeepCopy(), true
				},
				Secrets:   runtime.BrokerSecrets(userSecrets(values)),
				ListTools: runtime.MCPToolLister,
				Logger:    debugLogger(&logs),
			}

			var row db.Instance

			for range 3 {
				row, err = queries.GetInstance(t.Context(), instanceID)
				require.NoError(t, err)

				if row.Status == "ready" || row.Status == "failed" {
					break
				}

				require.NoError(t, starter.Step(t.Context(), row))
			}

			row, err = queries.GetInstance(t.Context(), instanceID)
			require.NoError(t, err)
			require.Equal(t, test.wantStatus, row.Status, row.Reason)
			assert.Equal(t, test.wantReason, row.Reason)

			if test.wantTools {
				assert.Contains(t, string(row.Tools), `"search"`)
				assert.NotEmpty(t, row.McpRevision)
			}

			// The list came from one user's credential: no catalog row
			// shares it with other users.
			catalog, err := queries.ListToolCatalogForTemplate(t.Context(), templateUID)
			require.NoError(t, err)
			assert.Empty(t, catalog)

			if test.key == "" {
				return
			}

			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(metrics.Collectors()...)

			families, err := registry.Gather()
			require.NoError(t, err)

			var exposition strings.Builder
			for _, family := range families {
				exposition.WriteString(family.String())
			}

			for _, text := range []string{logs.String(), row.Reason, row.UnhealthyReason, string(row.Tools), exposition.String()} {
				assert.NotContains(t, text, test.key)
			}
		})
	}
}

func TestTeardownStopsARemoteInstanceWithoutKubernetes(t *testing.T) {
	queries, startStore := catalogPool(t)

	_, err := queries.InsertInstance(t.Context(), db.InsertInstanceParams{
		ID: instanceID, TemplateNs: "catalog", TemplateName: "archive", TemplateUid: templateUID,
		Generation: 1, Scope: "per-user", ScopeKey: "user:alice", OwnerPrincipal: "alice",
	})
	require.NoError(t, err)

	_, err = startStore.MarkStarting(t.Context(), instanceID, 1, nil, time.Time{}, nil)
	require.NoError(t, err)
	_, err = startStore.MarkFailed(t.Context(), instanceID, "starting", "refused")
	require.NoError(t, err)

	// No Reader or Deleter: any Kubernetes call panics.
	teardown := &runtime.Teardown{Pool: startStore.Pool, Log: slog.New(slog.DiscardHandler), Metrics: runtime.NewMetrics()}

	for range 2 {
		row, err := queries.GetInstance(t.Context(), instanceID)
		require.NoError(t, err)

		_, err = teardown.Advance(t.Context(), row)
		require.NoError(t, err)
	}

	row, err := queries.GetInstance(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", row.Status)
}
