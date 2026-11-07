//go:build integration

package gateway_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const approvalBaseURL = "https://toolgate.example/ui/approvals/"

func withApprovalURL(options *gateway.MCPOptions) {
	options.ApprovalURL = func(id string) string { return approvalBaseURL + id }
}

// approvalFixture serves the server github whose echo tool needs approval,
// enabled and ready in alice's session.
type approvalFixture struct {
	*mcpFixture
	client   *observedClient
	template *v1alpha1.MCPServerTemplate
	instance db.Instance
}

func newApprovalFixture(t *testing.T, configure ...func(*gateway.MCPOptions)) *approvalFixture {
	t.Helper()

	fixture := newMCPFixture(t, configure...)
	template := fixture.addEnableTemplate("github", "per-user", v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"})
	template.Spec.Tools = map[string]v1alpha1.ToolOverride{"echo": {Approval: "required"}, "raw": {Approval: "required"}}
	fixture.catalog.Set(template, nil)
	fixture.startUpstream(t, "github")

	client := fixture.connectObserved(t, alice())
	instance := fixture.enableWithTools(t, client, "github", "user:alice", "echo", "raw")

	return &approvalFixture{mcpFixture: fixture, client: client, template: template, instance: instance}
}

func (f *mcpFixture) approvals(t *testing.T) []db.Approval {
	t.Helper()

	rows, err := f.pool.Query(t.Context(), `SELECT * FROM approvals ORDER BY created_at, id`)
	require.NoError(t, err)

	approvals, err := pgx.CollectRows(rows, pgx.RowToStructByPos[db.Approval])
	require.NoError(t, err)

	return approvals
}

// approve approves a pending approval as the UI does.
func (f *mcpFixture) approve(t *testing.T, id string) {
	t.Helper()

	_, err := f.pool.Exec(t.Context(), `UPDATE approvals SET status = 'approved', decided_at = now(), decided_by = principal,
		expires_at = now() + interval '10 minutes' WHERE id = $1 AND status = 'pending'`, id)
	require.NoError(t, err)
}

func pendingMessage(id string) string {
	return fmt.Sprintf("Tool `github__echo` needs approval. Approve request `%s` at %s%s, then call the tool again with the same arguments.", id, approvalBaseURL, id)
}

func TestApprovalRequest(t *testing.T) {
	t.Run("a call needing approval inserts a pending approval, writes approval.request and refuses with the link", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		result := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi", "a": 1})
		require.True(t, result.IsError)

		approvals := fixture.approvals(t)
		require.Len(t, approvals, 1)

		approval := approvals[0]
		assert.Equal(t, pendingMessage(approval.ID), resultText(result))
		assert.Equal(t, "pending", approval.Status)
		assert.Equal(t, "alice", approval.Principal)
		assert.Equal(t, "echo", approval.Tool)

		sum := sha256.Sum256([]byte(`{"a":1,"text":"hi"}`))
		assert.Equal(t, sum[:], approval.ArgumentsSha256)

		requests := fixture.auditRows(t, "approval.request")
		require.Len(t, requests, 1)
		assert.Equal(t, approval.ID, requests[0].Details["approval"])

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, "approval-pending", decisions[0].Details["reason"])
		assert.Equal(t, approval.ID, decisions[0].Details["approval"])
	})

	t.Run("without the web UI the call fails with the no web UI message", func(t *testing.T) {
		fixture := newApprovalFixture(t)

		result := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		require.True(t, result.IsError)
		assert.Equal(t, "Tool `github__echo` needs approval, but this gateway has no web UI. Ask an admin.", resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "approval-pending", decisions[0].Details["reason"])
	})

	t.Run("the same call again finds the pending approval", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		first := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		second := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})

		approvals := fixture.approvals(t)
		require.Len(t, approvals, 1)
		assert.Equal(t, pendingMessage(approvals[0].ID), resultText(first))
		assert.Equal(t, pendingMessage(approvals[0].ID), resultText(second))
		assert.Len(t, fixture.auditRows(t, "approval.request"), 1)

		// Other arguments are another call.
		callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "other"})
		assert.Len(t, fixture.approvals(t), 2)
	})

	t.Run("a usable approval lets the call through once and records it", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		id := fixture.approvals(t)[0].ID
		fixture.approve(t, id)

		result := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "hi", resultText(result))

		approval := fixture.approvals(t)[0]
		assert.True(t, approval.UsedAt.Valid)

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 2)
		assert.Equal(t, "allow", decisions[1].Details["decision"])
		assert.Equal(t, id, decisions[1].Details["approval"])
		assert.Equal(t, fixture.instance.ID, decisions[1].Details["instance"])
		assert.Len(t, fixture.auditRows(t, "tool.result"), 1)
	})

	t.Run("the upstream receives exactly the canonical bytes that were hashed", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		arguments := json.RawMessage(`{"text": "a", "n": 1.50, "text": "b"}`)
		canonical := `{"n":1.50,"text":"b"}`

		call := func() *mcp.CallToolResult {
			result, err := fixture.client.session.CallTool(t.Context(), &mcp.CallToolParams{Name: "github__raw", Arguments: arguments})
			require.NoError(t, err)

			return result
		}

		require.True(t, call().IsError)

		approval := fixture.approvals(t)[0]
		sum := sha256.Sum256([]byte(canonical))
		assert.Equal(t, sum[:], approval.ArgumentsSha256)
		fixture.approve(t, approval.ID)

		result := call()
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, canonical, resultText(result))
	})

	t.Run("a used approval lets no second call through; that call gets a new pending approval", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		first := fixture.approvals(t)[0].ID
		fixture.approve(t, first)

		require.False(t, callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"}).IsError)

		again := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		require.True(t, again.IsError)

		approvals := fixture.approvals(t)
		require.Len(t, approvals, 2)
		assert.NotEqual(t, first, approvals[1].ID)
		assert.Equal(t, "pending", approvals[1].Status)
		assert.Equal(t, pendingMessage(approvals[1].ID), resultText(again))
		assert.Len(t, fixture.auditRows(t, "tool.result"), 1)
	})

	t.Run("when another call uses the approval first, the call starts again once and gets a new pending approval", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		first := fixture.approvals(t)[0].ID
		fixture.approve(t, first)

		// The other call's compare-and-set commits between this call's
		// lookup and its use.
		t.Cleanup(gateway.SetBeforeApprovalUse(func() {
			_, err := fixture.pool.Exec(t.Context(), `UPDATE approvals SET used_at = now() WHERE id = $1`, first)
			assert.NoError(t, err)
		}))

		result := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		require.True(t, result.IsError)

		approvals := fixture.approvals(t)
		require.Len(t, approvals, 2)
		assert.Equal(t, pendingMessage(approvals[1].ID), resultText(result))
		assert.Empty(t, fixture.auditRows(t, "tool.result"))
	})

	t.Run("a RequestState naming another principal's approval finds nothing", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		bobClient := fixture.connectObserved(t, bob())
		fixture.enableWithTools(t, bobClient, "github", "user:bob", "echo")
		callTool(t, bobClient.session, "github__echo", map[string]any{"text": "hi"})
		bobs := fixture.approvals(t)[0].ID
		fixture.approve(t, bobs)

		result, err := fixture.client.session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "github__echo", Arguments: map[string]any{"text": "hi"}, RequestState: bobs,
		})
		require.NoError(t, err)
		require.True(t, result.IsError)

		approvals := fixture.approvals(t)
		require.Len(t, approvals, 2)
		assert.Equal(t, "alice", approvals[1].Principal)
		assert.Equal(t, pendingMessage(approvals[1].ID), resultText(result))
		assert.False(t, approvals[0].UsedAt.Valid, "bob's approval was used")
	})

	t.Run("a RequestState naming the caller's expired approval refuses with approval-timeout", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		id := fixture.approvals(t)[0].ID
		_, err := fixture.pool.Exec(t.Context(), `UPDATE approvals SET status = 'expired' WHERE id = $1`, id)
		require.NoError(t, err)

		result, err := fixture.client.session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "github__echo", Arguments: map[string]any{"text": "hi"}, RequestState: id,
		})
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("Nobody approved `github__echo` within 10 minutes (approval `%s`).", id), resultText(result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 2)
		assert.Equal(t, "approval-timeout", decisions[1].Details["reason"])
		assert.Empty(t, fixture.auditRows(t, "approval.expire"))
	})

	t.Run("once the template drops approval: required, the call needs no approval", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})

		template := fixture.template.DeepCopy()
		template.Spec.Tools = nil
		fixture.catalog.Set(template, nil)

		result := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		require.False(t, result.IsError, resultText(result))

		approvals := fixture.approvals(t)
		require.Len(t, approvals, 1)
		assert.Equal(t, "pending", approvals[0].Status)
	})
}

func TestApprovalPendingLimit(t *testing.T) {
	for _, test := range []struct {
		pending int
		refused bool
	}{{pending: 99}, {pending: 100, refused: true}} {
		t.Run(fmt.Sprintf("%d pending", test.pending), func(t *testing.T) {
			fixture := newApprovalFixture(t, withApprovalURL)

			session := fixture.sessionRow(t, "alice", "session-1")
			for number := range test.pending {
				_, err := fixture.pool.Exec(t.Context(), `INSERT INTO approvals (id, principal, session_pk, workspace, template_ns, template_name, tool, arguments_sha256, expires_at)
					VALUES ($1, 'alice', $2, '', $3, 'github', 'echo', $4, now() + interval '10 minutes')`,
					fmt.Sprintf("%032x", number+1), session.ID, templateNamespace, sha256.New().Sum([]byte(fmt.Sprint(number)))[:32])
				require.NoError(t, err)
			}

			result := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
			require.True(t, result.IsError)

			if test.refused {
				assert.Equal(t, "You have 100 approvals waiting. Approve or deny some first.", resultText(result))
				assert.Len(t, fixture.approvals(t), 100)

				return
			}

			assert.Contains(t, resultText(result), "needs approval. Approve request")
			assert.Len(t, fixture.approvals(t), 100)
		})
	}
}

func TestApprovalUseRechecks(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, fixture *approvalFixture)
		reason string
		step   string
	}{
		{
			name: "disabled server refuses with not-enabled",
			change: func(t *testing.T, fixture *approvalFixture) {
				_, err := fixture.pool.Exec(t.Context(), `DELETE FROM session_servers`)
				assert.NoError(t, err)
			},
			reason: "not-enabled",
		},
		{
			name: "ended session refuses with not-enabled",
			change: func(t *testing.T, fixture *approvalFixture) {
				_, err := fixture.pool.Exec(t.Context(), `UPDATE sessions SET ended_at = now()`)
				assert.NoError(t, err)
				_, err = fixture.pool.Exec(t.Context(), `DELETE FROM session_servers`)
				assert.NoError(t, err)
			},
			reason: "not-enabled",
		},
		{
			name: "a replacement instance that isn't ready refuses with not-ready",
			change: func(t *testing.T, fixture *approvalFixture) {
				_, err := fixture.pool.Exec(t.Context(), `UPDATE instances SET status = 'starting' WHERE id = $1`, fixture.instance.ID)
				assert.NoError(t, err)
			},
			reason: "not-ready",
		},
		{
			name: "failed instance refuses with not-ready",
			change: func(t *testing.T, fixture *approvalFixture) {
				_, err := fixture.pool.Exec(t.Context(), `UPDATE instances SET status = 'failed', reason = 'crashed' WHERE id = $1`, fixture.instance.ID)
				assert.NoError(t, err)
			},
			reason: "not-ready",
		},
		{
			name: "deleted template refuses with not-ready",
			change: func(_ *testing.T, fixture *approvalFixture) {
				fixture.catalog.Delete(types.NamespacedName{Namespace: templateNamespace, Name: "github"})
			},
			reason: "not-ready",
		},
		{
			name: "a rule that now refuses refuses with its step",
			change: func(t *testing.T, fixture *approvalFixture) {
				assert.NoError(t, fixture.rules.Set(blockServer("github")))
			},
			step: "rule",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newApprovalFixture(t, withApprovalURL)

			callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
			id := fixture.approvals(t)[0].ID
			fixture.approve(t, id)

			t.Cleanup(gateway.SetBeforeApprovalUse(func() { test.change(t, fixture) }))

			result := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
			require.True(t, result.IsError)

			approval := fixture.approvals(t)[0]
			assert.False(t, approval.UsedAt.Valid, "the refusal didn't roll back used_at")

			decisions := fixture.auditRows(t, "tool.decision")
			last := decisions[len(decisions)-1]
			assert.Equal(t, "refuse", last.Details["decision"])

			if test.step != "" {
				assert.Equal(t, test.step, last.Details["step"])
			} else {
				assert.Equal(t, test.reason, last.Details["reason"])
			}

			assert.Equal(t, id, last.Details["approval"])
			assert.Empty(t, fixture.auditRows(t, "tool.result"))
		})
	}
}
