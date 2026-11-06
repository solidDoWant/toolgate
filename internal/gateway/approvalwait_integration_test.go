//go:build integration

package gateway_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// revisionElicitCreate is the protocol revision on which the go-sdk's
// server middleware sends elicitation/create itself.
const revisionElicitCreate = "2025-11-25"

// approvalWakeBound is how soon a waiting call must notice an approval made
// with a notification. It is below the 10 s tick, so a wait that wakes only
// on the tick fails the test.
const approvalWakeBound = 8 * time.Second

// elicitingClient is an MCP client that declares URL elicitation and
// answers every elicitation with answer.
type elicitingClient struct {
	session *mcp.ClientSession

	mu       sync.Mutex
	elicited []*mcp.ElicitParams
	progress []string
	// onElicit, when set, runs before the answer is returned.
	onElicit func(params *mcp.ElicitParams)
}

func (c *elicitingClient) elicitations() []*mcp.ElicitParams {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]*mcp.ElicitParams(nil), c.elicited...)
}

func (c *elicitingClient) progressMessages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.progress...)
}

type elicitingOptions struct {
	revision string
	answer   string
}

// connectEliciting connects alice's session with a client that declares
// URL elicitation, and lists tools so the MCP session offers the proxied
// tools.
func (f *mcpFixture) connectEliciting(t *testing.T, options elicitingOptions) *elicitingClient {
	t.Helper()

	return f.connectElicitingAs(t, alice(), options)
}

// connectElicitingAs is connectEliciting for user.
func (f *mcpFixture) connectElicitingAs(t *testing.T, user user, options elicitingOptions) *elicitingClient {
	t.Helper()

	client := &elicitingClient{}

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "eliciting", Version: "0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}},
		ElicitationHandler: func(_ context.Context, request *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			client.mu.Lock()
			client.elicited = append(client.elicited, request.Params)
			hook := client.onElicit
			client.mu.Unlock()

			if hook != nil {
				hook(request.Params)
			}

			return &mcp.ElicitResult{Action: options.answer}, nil
		},
		ProgressNotificationHandler: func(_ context.Context, request *mcp.ProgressNotificationClientRequest) {
			client.mu.Lock()
			client.progress = append(client.progress, request.Params.Message)
			client.mu.Unlock()
		},
	})
	if options.revision >= modernRevision {
		mcpClient.AddSendingMiddleware(skipDiscover)
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:   f.server.URL,
		HTTPClient: &http.Client{Transport: headerTransport{header: f.headers(t, user)}},
	}

	session, err := mcpClient.Connect(t.Context(), transport, &mcp.ClientSessionOptions{ProtocolVersion: options.revision})
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	client.session = session

	// The test proves the revision it asks for, not a fallback.
	require.Equal(t, options.revision, session.InitializeResult().ProtocolVersion)

	_, err = session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	return client
}

// approveAndNotify approves a pending approval and notifies approval:<id>,
// as the UI handler on another replica does.
func (f *mcpFixture) approveAndNotify(t *testing.T, id string) {
	t.Helper()

	f.setApprovalStatus(t, id, "approved")
}

// setApprovalStatus moves a pending approval to status and notifies
// approval:<id> in the same transaction.
func (f *mcpFixture) setApprovalStatus(t *testing.T, id, status string) {
	t.Helper()

	_, err := f.pool.Exec(context.Background(), `WITH changed AS (
			UPDATE approvals SET status = $2, decided_at = now(), decided_by = principal,
				expires_at = CASE WHEN $2 = 'approved' THEN now() + interval '10 minutes' ELSE expires_at END
			WHERE id = $1 AND status = 'pending' RETURNING id)
		SELECT pg_notify('toolgate_events', 'approval:' || id) FROM changed`, id, status)
	require.NoError(t, err)
}

// awaitPendingApproval waits until alice has a pending approval and
// returns it.
func (f *mcpFixture) awaitPendingApproval(t *testing.T) db.Approval {
	t.Helper()

	var approval db.Approval

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		approvals := f.approvals(t)
		if assert.Len(collect, approvals, 1) {
			approval = approvals[0]
		}
	}, waitTimeout, 20*time.Millisecond)

	return approval
}

type callOutcome struct {
	result *mcp.CallToolResult
	err    error
}

func startEcho(ctx context.Context, session *mcp.ClientSession, params *mcp.CallToolParams) <-chan callOutcome {
	outcomes := make(chan callOutcome, 1)

	go func() {
		result, err := session.CallTool(ctx, params)
		outcomes <- callOutcome{result: result, err: err}
	}()

	return outcomes
}

func echoParams() *mcp.CallToolParams {
	params := &mcp.CallToolParams{Name: "github__echo", Arguments: map[string]any{"text": "hi"}}
	params.SetProgressToken("echo")

	return params
}

func awaitOutcome(t *testing.T, outcomes <-chan callOutcome, bound time.Duration) callOutcome {
	t.Helper()

	select {
	case outcome := <-outcomes:
		return outcome
	case <-time.After(bound):
		require.FailNow(t, "the call did not finish in time")

		return callOutcome{}
	}
}

func TestApprovalElicitation(t *testing.T) {
	for _, revision := range []string{revisionElicitCreate, modernRevision} {
		t.Run("row 40: on "+revision+" the client opens the approval page, an approval from another replica lets the call through", func(t *testing.T) {
			fixture := newApprovalFixture(t, withApprovalURL)
			client := fixture.connectEliciting(t, elicitingOptions{revision: revision, answer: "accept"})

			started := time.Now()
			outcomes := startEcho(t.Context(), client.session, echoParams())

			approval := fixture.awaitPendingApproval(t)
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				assert.NotEmpty(collect, client.progressMessages())
			}, waitTimeout, 20*time.Millisecond)

			// The approval commits elsewhere, as on another replica.
			fixture.approveAndNotify(t, approval.ID)

			outcome := awaitOutcome(t, outcomes, approvalWakeBound)
			require.NoError(t, outcome.err)
			require.False(t, outcome.result.IsError, resultText(outcome.result))
			assert.Equal(t, "hi", resultText(outcome.result))
			assert.Less(t, time.Since(started), approvalWakeBound)

			elicitations := client.elicitations()
			require.Len(t, elicitations, 1)
			assert.Equal(t, "url", elicitations[0].Mode)
			assert.Equal(t, approvalBaseURL+approval.ID, elicitations[0].URL)
			assert.Equal(t, approval.ID, elicitations[0].ElicitationID)

			used := fixture.approvals(t)[0]
			assert.True(t, used.UsedAt.Valid)

			decisions := fixture.auditRows(t, "tool.decision")
			require.Len(t, decisions, 1)
			assert.Equal(t, "allow", decisions[0].Details["decision"])
			assert.Equal(t, approval.ID, decisions[0].Details["approval"])
		})

		for _, answer := range []string{"decline", "cancel"} {
			t.Run("on "+revision+" a "+answer+" answer fails the call with the approval link", func(t *testing.T) {
				fixture := newApprovalFixture(t, withApprovalURL)
				client := fixture.connectEliciting(t, elicitingOptions{revision: revision, answer: answer})

				result := callTool(t, client.session, "github__echo", map[string]any{"text": "hi"})

				approvals := fixture.approvals(t)
				require.Len(t, approvals, 1)
				assert.Equal(t, "pending", approvals[0].Status)
				require.True(t, result.IsError)
				assert.Equal(t, pendingMessage(approvals[0].ID), resultText(result))
				assert.Len(t, client.elicitations(), 1)

				decisions := fixture.auditRows(t, "tool.decision")
				require.Len(t, decisions, 1)
				assert.Equal(t, "approval-pending", decisions[0].Details["reason"])
			})
		}
	}

	t.Run("a request carrying input responses never gets input requests back", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)
		client := fixture.connectEliciting(t, elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		result, err := client.session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "github__echo", Arguments: map[string]any{"text": "hi"},
			InputResponses: mcp.InputResponseMap{"other": &mcp.ElicitResult{Action: "accept"}},
		})
		require.NoError(t, err)
		assert.False(t, result.NeedsInput())
		assert.Nil(t, result.InputRequests)

		approval := fixture.awaitPendingApproval(t)
		require.True(t, result.IsError)
		assert.Equal(t, pendingMessage(approval.ID), resultText(result))
		assert.Empty(t, client.elicitations())
	})

	t.Run("a client without URL elicitation gets the approval link", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)

		result := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		require.True(t, result.IsError)
		assert.Equal(t, pendingMessage(fixture.approvals(t)[0].ID), resultText(result))
	})
}

func TestApprovalWait(t *testing.T) {
	t.Run("row 40: a denial fails the waiting call", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)
		client := fixture.connectEliciting(t, elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		outcomes := startEcho(t.Context(), client.session, echoParams())
		approval := fixture.awaitPendingApproval(t)
		fixture.awaitWaiting(t, client)
		fixture.setApprovalStatus(t, approval.ID, "denied")

		outcome := awaitOutcome(t, outcomes, approvalWakeBound)
		require.NoError(t, outcome.err)
		require.True(t, outcome.result.IsError)
		assert.Equal(t, "Approval `"+approval.ID+"` was denied.", resultText(outcome.result))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "approval-denied", decisions[0].Details["reason"])
		assert.Equal(t, approval.ID, decisions[0].Details["approval"])
	})

	t.Run("row 41: when expires_at passes, the waiting call expires the approval and fails with the timeout in words", func(t *testing.T) {
		t.Cleanup(gateway.SetApprovalWaitInterval(100 * time.Millisecond))

		fixture := newApprovalFixture(t, withApprovalURL, func(options *gateway.MCPOptions) { options.ApprovalTimeout = time.Minute })
		client := fixture.connectEliciting(t, elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		outcomes := startEcho(t.Context(), client.session, echoParams())
		approval := fixture.awaitPendingApproval(t)
		fixture.awaitWaiting(t, client)

		_, err := fixture.pool.Exec(t.Context(), `UPDATE approvals SET expires_at = now() WHERE id = $1`, approval.ID)
		require.NoError(t, err)

		outcome := awaitOutcome(t, outcomes, approvalWakeBound)
		require.NoError(t, outcome.err)
		require.True(t, outcome.result.IsError)
		assert.Equal(t, "Nobody approved `github__echo` within 1 minute (approval `"+approval.ID+"`).", resultText(outcome.result))

		assert.Equal(t, "expired", fixture.approvals(t)[0].Status)
		expires := fixture.auditRows(t, "approval.expire")
		require.Len(t, expires, 1)
		assert.Equal(t, approval.ID, expires[0].Details["approval"])

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "approval-timeout", decisions[0].Details["reason"])
	})

	t.Run("row 41: when the leader's pass expired the approval, the waiting call refuses without approval.expire", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)
		client := fixture.connectEliciting(t, elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		outcomes := startEcho(t.Context(), client.session, echoParams())
		approval := fixture.awaitPendingApproval(t)
		fixture.awaitWaiting(t, client)
		fixture.setApprovalStatus(t, approval.ID, "expired")

		outcome := awaitOutcome(t, outcomes, approvalWakeBound)
		require.NoError(t, outcome.err)
		require.True(t, outcome.result.IsError)
		assert.Equal(t, "Nobody approved `github__echo` within 10 minutes (approval `"+approval.ID+"`).", resultText(outcome.result))
		assert.Empty(t, fixture.auditRows(t, "approval.expire"))

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "approval-timeout", decisions[0].Details["reason"])
	})

	t.Run("rows 41 and 42: every write is a compare-and-set on pending, and the first commit wins", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)
		callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		id := fixture.approvals(t)[0].ID

		queries := db.New(fixture.pool)
		_, err := fixture.pool.Exec(t.Context(), `UPDATE approvals SET expires_at = now() + interval '1 minute' WHERE id = $1`, id)
		require.NoError(t, err)

		_, err = queries.ApproveApproval(t.Context(), db.ApproveApprovalParams{ID: id, DecidedBy: pgtype.Text{String: "alice", Valid: true}, ApprovalTimeout: minutes(10)})
		require.NoError(t, err)

		// The expiring write and a deny in another tab match nothing.
		_, err = fixture.pool.Exec(t.Context(), `UPDATE approvals SET expires_at = now() WHERE id = $1`, id)
		require.NoError(t, err)
		_, err = queries.ExpireDueApproval(t.Context(), id)
		require.ErrorIs(t, err, errNoRows)
		_, err = queries.DenyApproval(t.Context(), db.DenyApprovalParams{ID: id, DecidedBy: pgtype.Text{String: "alice", Valid: true}})
		require.ErrorIs(t, err, errNoRows)
		assert.Equal(t, "approved", fixture.approvals(t)[0].Status)
	})

	t.Run("row 43: a rule that refuses the call ends the wait with the refusal", func(t *testing.T) {
		t.Cleanup(gateway.SetApprovalWaitInterval(100 * time.Millisecond))

		fixture := newApprovalFixture(t, withApprovalURL)
		client := fixture.connectEliciting(t, elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		outcomes := startEcho(t.Context(), client.session, echoParams())
		approval := fixture.awaitPendingApproval(t)
		fixture.awaitWaiting(t, client)

		require.NoError(t, fixture.rules.Set(blockToolFor("alice", "github__echo", "Echo is off for alice.")))

		outcome := awaitOutcome(t, outcomes, approvalWakeBound)
		require.NoError(t, outcome.err)
		require.True(t, outcome.result.IsError)
		assert.Contains(t, resultText(outcome.result), "Echo is off for alice.")

		decisions := fixture.auditRows(t, "tool.decision")
		require.Len(t, decisions, 1)
		assert.Equal(t, "refuse", decisions[0].Details["decision"])
		assert.Equal(t, approval.ID, decisions[0].Details["approval"])
		assert.Equal(t, "pending", fixture.approvals(t)[0].Status)
	})

	t.Run("row 44: a cancelled wait writes nothing, and a retry uses the approval once approved", func(t *testing.T) {
		fixture := newApprovalFixture(t, withApprovalURL)
		client := fixture.connectEliciting(t, elicitingOptions{revision: revisionElicitCreate, answer: "accept"})

		ctx, cancel := context.WithCancel(t.Context())
		outcomes := startEcho(ctx, client.session, echoParams())
		approval := fixture.awaitPendingApproval(t)
		fixture.awaitWaiting(t, client)

		cancel()

		outcome := awaitOutcome(t, outcomes, waitTimeout)
		require.Error(t, outcome.err)

		assert.Empty(t, fixture.auditRows(t, "tool.decision"))
		assert.Equal(t, "pending", fixture.approvals(t)[0].Status)

		fixture.approveAndNotify(t, approval.ID)

		result := callTool(t, fixture.client.session, "github__echo", map[string]any{"text": "hi"})
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, "hi", resultText(result))
	})
}

// awaitWaiting waits until the call of client has started waiting: it got
// a progress notification about its approval.
func (f *mcpFixture) awaitWaiting(t *testing.T, client *elicitingClient) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.NotEmpty(collect, client.progressMessages())
	}, waitTimeout, 20*time.Millisecond)
}

func minutes(count int64) pgtype.Interval {
	return pgtype.Interval{Microseconds: count * time.Minute.Microseconds(), Valid: true}
}

var errNoRows = pgx.ErrNoRows
