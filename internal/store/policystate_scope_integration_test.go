//go:build integration

package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestLoadPolicyStateScope(t *testing.T) {
	pg := testutil.StartPostgres(t)
	pool := openPool(t, pg.ConnString())
	ctx := t.Context()

	require.NoError(t, store.Migrate(ctx, pool))

	_, err := pool.Exec(ctx, `INSERT INTO user_scopes (principal, verified_default, unverified_default) VALUES ('alice', 'all', 'none'), ('carol', 'none', 'all')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO workspace_lists (scope_id, workspace) SELECT id, w FROM user_scopes, unnest(ARRAY['coder/alice/mail', 'coder/alice/empty']) AS w WHERE principal = 'alice'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO workspace_list_servers (list_id, server) SELECT id, s FROM workspace_lists, unnest(ARRAY['mail', 'calendar']) AS s WHERE workspace = 'coder/alice/mail'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO sessions (principal, session_id, workspace, attestation, kind) VALUES ('alice', 'session-1', 'coder/alice/mail', 'attested', 'user'), ('bob', 'session-1', 'w', 'user', 'user')`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO session_additions (session_pk, server, request_id) SELECT id, 'immich', repeat('a', 32) FROM sessions WHERE principal = 'alice'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO session_additions (session_pk, server, request_id) SELECT id, 'k8s', repeat('b', 32) FROM sessions WHERE principal = 'bob'`)
	require.NoError(t, err)

	tests := []struct {
		name      string
		principal string
		query     store.ScopeQuery
		want      policy.ScopeRows
	}{
		{
			name:      "nothing asked reads nothing",
			principal: "alice",
			want:      policy.ScopeRows{},
		},
		{
			name:      "a verified workspace with a list reads the defaults, the list and the session's additions",
			principal: "alice",
			query:     store.ScopeQuery{Load: true, Workspace: "coder/alice/mail", Session: "session-1"},
			want: policy.ScopeRows{
				VerifiedDefault: "all", UnverifiedDefault: "none",
				HasList: true, ListServers: []string{"calendar", "mail"}, Additions: []string{"immich"},
			},
		},
		{
			name:      "an empty list is a list with no servers",
			principal: "alice",
			query:     store.ScopeQuery{Load: true, Workspace: "coder/alice/empty", Session: "session-1"},
			want:      policy.ScopeRows{VerifiedDefault: "all", UnverifiedDefault: "none", HasList: true, Additions: []string{"immich"}},
		},
		{
			name:      "a workspace without a list has none",
			principal: "alice",
			query:     store.ScopeQuery{Load: true, Workspace: "coder/alice/other", Session: "session-1"},
			want:      policy.ScopeRows{VerifiedDefault: "all", UnverifiedDefault: "none", Additions: []string{"immich"}},
		},
		{
			name:      "an unverified request reads only the defaults",
			principal: "alice",
			query:     store.ScopeQuery{Load: true},
			want:      policy.ScopeRows{VerifiedDefault: "all", UnverifiedDefault: "none"},
		},
		{
			name:      "another principal's list and additions are never read",
			principal: "carol",
			query:     store.ScopeQuery{Load: true, Workspace: "coder/alice/mail", Session: "session-1"},
			want:      policy.ScopeRows{VerifiedDefault: "none", UnverifiedDefault: "all"},
		},
		{
			name:      "a missing user_scopes row leaves both defaults empty",
			principal: "dave",
			query:     store.ScopeQuery{Load: true},
			want:      policy.ScopeRows{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, err := store.LoadPolicyState(t.Context(), pool, test.principal, false, test.query)
			require.NoError(t, err)
			assert.Equal(t, test.want, rows.Scope)
		})
	}
}
