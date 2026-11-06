//go:build integration

package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Row 251: a template named `toolgate` from before the upgrade counts as
// deleted and "the name means the built-in server". A session that had the
// old template enabled (rows outlive a template deletion) can't use the
// built-in tools.
func TestBughuntReservedNameOldEnabledRowShadowsBuiltin(t *testing.T) {
	fixture := newBuiltinFixture(t)
	fixture.setVerifiedDefault(t, policy.ScopeDefaultAll)
	client := fixture.connectObserved(t, attestedAlice())

	// Before the upgrade, the session enabled the real template `toolgate`.
	sessionPK := fixture.sessionRow(t, "alice", "session-1").ID
	require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
		SessionPk: sessionPK, TemplateNs: templateNamespace, TemplateName: "toolgate",
	}))

	result := callTool(t, client.session, toolListScopes, nil)
	assert.False(t, result.IsError, resultText(result))
}
