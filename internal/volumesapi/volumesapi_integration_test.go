//go:build integration

package volumesapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/volumesapi"
)

const adminGroup = "toolgate-admins"

type fixture struct {
	pool     *pgxpool.Pool
	provider *testutil.OIDCProvider
	url      string
	audit    *bytes.Buffer
}

func newFixture(t *testing.T, group string) fixture {
	t.Helper()

	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	authenticator, err := auth.New(t.Context(), auth.Options{IssuerURL: provider.Issuer, Audiences: []string{provider.Audience}})
	require.NoError(t, err)

	output := &bytes.Buffer{}
	mux := http.NewServeMux()
	(&volumesapi.Handler{
		Queries: db.New(pool), Pool: pool, AdminGroup: group, Output: output, Log: slog.New(slog.DiscardHandler),
	}).Register(mux, func(next http.Handler) http.Handler { return authenticator.Middleware(next, false) })

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return fixture{pool: pool, provider: provider, url: server.URL, audit: output}
}

// seed inserts a volume row with a fixed ID and returns the ID.
func (f fixture) seed(t *testing.T, index int, scope, scopeID string, held bool) string {
	t.Helper()

	id := fmt.Sprintf("%032x", index)
	holder := any(nil)

	if held {
		holder = fmt.Sprintf("%032x", 1000+index)
		_, err := f.pool.Exec(t.Context(), `INSERT INTO instances (id, template_ns, template_name, template_uid, generation, scope, scope_key, namespace)
			VALUES ($1, 'catalog', 'eparts', 'uid', 1, 'per-user', 'k', 'ns')`, holder)
		require.NoError(t, err)
	}

	pvName := any(nil)
	if index%2 == 0 {
		pvName = fmt.Sprintf("pvc-%d", index)
	}

	_, err := f.pool.Exec(t.Context(), `INSERT INTO persistent_volumes
		(id, template_ns, template_name, template_uid, volume_name, scope, scope_id, storage_class, size_bytes, pv_name, instance_id)
		VALUES ($1, 'catalog', 'eparts', 'uid', $2, $3, $4, 'ssd', 1073741824, $5, $6)`,
		id, fmt.Sprintf("cache%d", index), scope, scopeID, pvName, holder)
	require.NoError(t, err)

	return id
}

func (f fixture) call(t *testing.T, method, path, subject string, groups ...string) (int, []byte) {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), method, f.url+path, nil)
	require.NoError(t, err)

	if subject != "" {
		request.Header.Set("Authorization", "Bearer "+f.provider.MintAccessToken(t, testutil.TokenClaims{
			Subject: subject, Email: subject + "@example.com", EmailVerified: true, Groups: groups,
		}))
	}

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response.StatusCode, body
}

func (f fixture) list(t *testing.T, query string) volumesapi.List {
	t.Helper()

	status, body := f.call(t, http.MethodGet, volumesapi.Path+query, "root", adminGroup)
	require.Equal(t, http.StatusOK, status, string(body))

	var list volumesapi.List
	require.NoError(t, json.Unmarshal(body, &list), string(body))

	return list
}

func TestAdminVolumesAPI(t *testing.T) {
	f := newFixture(t, adminGroup)

	alice := f.seed(t, 2, "per-user", "alice", true)
	f.seed(t, 3, "per-user", "bob", false)
	template := f.seed(t, 4, "per-template", "", false)

	t.Run("a request without a token is refused", func(t *testing.T) {
		status, _ := f.call(t, http.MethodGet, volumesapi.Path, "")
		assert.Equal(t, http.StatusUnauthorized, status)
	})

	t.Run("a user outside the admin group is refused", func(t *testing.T) {
		status, _ := f.call(t, http.MethodGet, volumesapi.Path, "alice", "eng")
		assert.Equal(t, http.StatusForbidden, status)

		status, _ = f.call(t, http.MethodDelete, volumesapi.Path+"/"+alice, "alice", "eng")
		assert.Equal(t, http.StatusForbidden, status)

		list := f.list(t, "")
		for _, volume := range list.Volumes {
			assert.Empty(t, volume.DeleteReason, "a refused delete marks nothing")
		}
	})

	t.Run("the list shows the rows in ID order in the documented shape", func(t *testing.T) {
		list := f.list(t, "")
		require.Len(t, list.Volumes, 3)
		assert.Empty(t, list.Next)

		assert.Equal(t, alice, list.Volumes[0].ID)
		assert.Equal(t, "alice", list.Volumes[0].Owner)
		assert.Equal(t, "eparts", list.Volumes[0].Server)
		assert.Equal(t, "catalog", list.Volumes[0].TemplateNamespace)
		assert.Equal(t, "per-user", list.Volumes[0].Scope)
		assert.EqualValues(t, 1073741824, list.Volumes[0].SizeBytes)
		require.NotNil(t, list.Volumes[0].InUseBy)
		require.NotNil(t, list.Volumes[0].PersistentVolume)
		assert.Equal(t, "pvc-2", *list.Volumes[0].PersistentVolume)

		assert.Nil(t, list.Volumes[1].InUseBy)
		assert.Nil(t, list.Volumes[1].PersistentVolume)

		assert.Equal(t, template, list.Volumes[2].ID)
		assert.Empty(t, list.Volumes[2].Owner, "a per-template volume has no owner")

		status, body := f.call(t, http.MethodGet, volumesapi.Path, "root", adminGroup)
		require.Equal(t, http.StatusOK, status)
		assert.Contains(t, string(body), `"inUseBy":null`)
		assert.NotContains(t, string(body), `"next"`)
	})

	t.Run("the owner filter keeps only that user's volumes", func(t *testing.T) {
		list := f.list(t, "?owner=bob")
		require.Len(t, list.Volumes, 1)
		assert.Equal(t, "bob", list.Volumes[0].Owner)

		assert.Empty(t, f.list(t, "?owner=nobody").Volumes)
	})

	t.Run("an over-long owner and a malformed after value are refused", func(t *testing.T) {
		status, _ := f.call(t, http.MethodGet, volumesapi.Path+"?owner="+strings.Repeat("a", volumesapi.MaxOwnerLength+1), "root", adminGroup)
		assert.Equal(t, http.StatusBadRequest, status)

		status, _ = f.call(t, http.MethodGet, volumesapi.Path+"?after=zz", "root", adminGroup)
		assert.Equal(t, http.StatusBadRequest, status)
	})

	t.Run("deleting a held volume marks it, answers 202 with the holder and writes the audit event", func(t *testing.T) {
		status, body := f.call(t, http.MethodDelete, volumesapi.Path+"/"+alice, "root", adminGroup)
		require.Equal(t, http.StatusAccepted, status, string(body))

		var volume volumesapi.Volume
		require.NoError(t, json.Unmarshal(body, &volume))
		assert.Equal(t, "admin", volume.DeleteReason)
		require.NotNil(t, volume.InUseBy)

		var principal, server string

		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT principal, server FROM audit_events WHERE type = 'volume.delete'`).Scan(&principal, &server))
		assert.Equal(t, "root", principal)
		assert.Equal(t, "eparts", server)

		var requested bool

		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT delete_requested_at IS NOT NULL FROM persistent_volumes WHERE id = $1`, alice).Scan(&requested))
		assert.True(t, requested)
	})

	t.Run("deleting a volume that is already being deleted answers 202 and changes nothing", func(t *testing.T) {
		var before string

		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT delete_requested_at::text FROM persistent_volumes WHERE id = $1`, alice).Scan(&before))

		status, _ := f.call(t, http.MethodDelete, volumesapi.Path+"/"+alice, "root", adminGroup)
		assert.Equal(t, http.StatusAccepted, status)

		var after string

		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT delete_requested_at::text FROM persistent_volumes WHERE id = $1`, alice).Scan(&after))
		assert.Equal(t, before, after)

		var events int

		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE type = 'volume.delete'`).Scan(&events))
		assert.Equal(t, 1, events, "the second delete writes no event")
	})

	t.Run("an unknown or malformed ID answers 404", func(t *testing.T) {
		status, _ := f.call(t, http.MethodDelete, volumesapi.Path+"/"+fmt.Sprintf("%032x", 999), "root", adminGroup)
		assert.Equal(t, http.StatusNotFound, status)

		status, _ = f.call(t, http.MethodDelete, volumesapi.Path+"/nope", "root", adminGroup)
		assert.Equal(t, http.StatusNotFound, status)
	})

	t.Run("the list pages by 200 rows with next on all but the last page", func(t *testing.T) {
		paged := newFixture(t, adminGroup)

		for index := 1; index <= volumesapi.PageSize+1; index++ {
			paged.seed(t, index, "per-template", "", false)
		}

		first := paged.list(t, "")
		require.Len(t, first.Volumes, volumesapi.PageSize)
		assert.Equal(t, first.Volumes[volumesapi.PageSize-1].ID, first.Next)

		second := paged.list(t, "?after="+first.Next)
		require.Len(t, second.Volumes, 1)
		assert.Empty(t, second.Next)
	})
}

func TestAdminVolumesAPIWithoutAdminGroupRefusesEveryone(t *testing.T) {
	f := newFixture(t, "")

	status, _ := f.call(t, http.MethodGet, volumesapi.Path, "root", adminGroup)
	assert.Equal(t, http.StatusForbidden, status)

	status, _ = f.call(t, http.MethodDelete, volumesapi.Path+"/"+fmt.Sprintf("%032x", 1), "root", adminGroup)
	assert.Equal(t, http.StatusForbidden, status)
}
