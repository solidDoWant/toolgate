//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// e2eApprovalTimeout is config.ui.approvalTimeout in the e2e install.
const e2eApprovalTimeout = time.Minute

// approvalCallBound is how long a call waiting for an approval nobody acts
// on may take: the timeout plus the 10 s re-read tick and some slack.
const approvalCallBound = e2eApprovalTimeout + 45*time.Second

// leaderExpiryBound is how long a pending approval nobody waits for may stay
// pending: the leader's pass expires it once expires_at is more than 1
// minute past, and the pass runs every minute.
const leaderExpiryBound = e2eApprovalTimeout + 3*time.Minute

const eventElicitation = "elicitation"

var approvalTimeoutPattern = regexp.MustCompile("^Nobody approved `([^`]+)` within 1 minute \\(approval `([0-9a-f]{32})`\\)\\.$")

// connectEliciting is connectGateway with a client that declares URL
// elicitation and accepts every elicitation without approving anything, as
// a user who never opens the page.
func connectEliciting(t *testing.T, endpoint, user, session string) *gatewaySession {
	t.Helper()

	observed := &gatewaySession{}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e-eliciting", Version: "0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}},
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			observed.record(eventElicitation)

			return &mcp.ElicitResult{Action: "accept"}, nil
		},
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { observed.record(eventListChanged) },
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) {
			observed.record(eventProgress)
		},
	})

	connected, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearerTransport{header: gatewayHeader(t, user, session, nil)}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = connected.Close() })

	observed.session = connected

	return observed
}

// createApprovalTemplate creates a per-user template for alice whose echo
// tool needs approval.
func createApprovalTemplate(t *testing.T) string {
	t.Helper()

	image := mcpServerImage(t)
	kube := templateClient(t)

	name := "e2e-timeout-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "per-user",
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       v1alpha1.Access{Users: []string{"alice"}, Environments: "any"},
			Tools:        map[string]v1alpha1.ToolOverride{"echo": {Approval: "required"}},
			Endpoint:     v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources:    testServerResources(t, image, 0, nil),
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))
	t.Cleanup(func() { _ = kube.Delete(context.WithoutCancel(t.Context()), template) })
	requireTemplateValid(t, kube, name)

	return name
}

type callResult struct {
	result *mcp.CallToolResult
	err    error
}

func startCall(ctx context.Context, session *mcp.ClientSession, params *mcp.CallToolParams) <-chan callResult {
	done := make(chan callResult, 1)

	go func() {
		result, err := session.CallTool(ctx, params)
		done <- callResult{result: result, err: err}
	}()

	return done
}

// requireWaiting waits until the gateway sent session an elicitation and,
// after it, a progress notification of the wait.
func requireWaiting(t *testing.T, session *gatewaySession) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		events := session.recorded()
		elicited := slices.Index(events, eventElicitation)
		require.GreaterOrEqual(collect, elicited, 0, "no elicitation yet")
		assert.Contains(collect, events[elicited:], eventProgress, "the call doesn't wait yet")
	}, 30*time.Second, 200*time.Millisecond)
}

func TestAC2_13ApprovalTimeout(t *testing.T) {
	admin := adminClient(t)
	name := createApprovalTemplate(t)

	endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
	alice := connectEliciting(t, endpoint, "alice", "e2e-"+rand.String(8))
	enable(t, alice, name)

	t.Run("[AC-2-13] an approval nobody acts on fails the call after the approval timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), approvalCallBound+time.Minute)
		defer cancel()

		started := time.Now()
		done := startCall(ctx, alice.session, &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": "nobody approves"}})

		requireWaiting(t, alice)

		var outcome callResult
		select {
		case outcome = <-done:
		case <-time.After(approvalCallBound):
			require.FailNow(t, "the call still waits after the approval timeout")
		}

		require.NoError(t, outcome.err)
		require.True(t, outcome.result.IsError)

		match := approvalTimeoutPattern.FindStringSubmatch(callText(outcome.result))
		require.NotNil(t, match, "the call fails with the timeout message: %s", callText(outcome.result))
		assert.Equal(t, name+"__echo", match[1])
		assert.GreaterOrEqual(t, time.Since(started), e2eApprovalTimeout, "the call waited for the approval timeout")
	})
}

func TestApprovalWaitReplicaDies(t *testing.T) {
	admin := adminClient(t)
	name := createApprovalTemplate(t)

	// The waiting call runs on a replica that doesn't run the leader's
	// pass, so the pass keeps running when that replica dies.
	leader := runningLeader(t, admin)
	pods := gatewayPods(t, admin)
	require.Len(t, pods, 2)

	waiter := pods[0]
	if waiter == leader {
		waiter = pods[1]
	}

	alice := connectEliciting(t, forwardToGateway(t, admin, waiter), "alice", "e2e-"+rand.String(8))
	enable(t, alice, name)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	call := &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": "the replica dies"}}
	done := startCall(ctx, alice.session, call)

	requireWaiting(t, alice)

	require.NoError(t, admin.CoreV1().Pods(releaseNamespace()).Delete(t.Context(), waiter,
		metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}))

	// The client may try to resume the dead replica's stream; stop it.
	cancel()
	<-done

	// The retry carries RequestState naming the approval, as a client
	// that retries the input-required call does.
	survivor := connectGateway(t, forwardToGateway(t, admin, leader), "alice", "e2e-"+rand.String(8))
	enable(t, survivor, name)

	var id string

	t.Run("the leader's pass expires the approval and a retry naming it gets the timeout message", func(t *testing.T) {
		id = pendingApprovalID(t, survivor, call)

		retry := *call
		retry.RequestState = id

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			result, err := survivor.session.CallTool(t.Context(), &retry)
			require.NoError(collect, err)
			require.True(collect, result.IsError)

			match := approvalTimeoutPattern.FindStringSubmatch(callText(result))
			require.NotNil(collect, match, "the approval is not expired yet: %s", callText(result))
			assert.Equal(collect, id, match[2])
		}, leaderExpiryBound, 10*time.Second)
	})
}

// pendingApprovalID calls the tool without elicitation, which finds the
// call's pending approval, and returns its ID from the error.
func pendingApprovalID(t *testing.T, session *gatewaySession, call *mcp.CallToolParams) string {
	t.Helper()

	result, err := session.session.CallTool(t.Context(), call)
	require.NoError(t, err)
	require.True(t, result.IsError)

	match := approvalLinkPattern.FindStringSubmatch(callText(result))
	require.NotNil(t, match, "the call names its pending approval: %s", callText(result))

	return match[1]
}
