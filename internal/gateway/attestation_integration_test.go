//go:build integration

package gateway_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/solidDoWant/toolgate/internal/auth"
)

const (
	workloadToken = "workload-token"
	// rotatedWorkloadToken is a later token bound to the same pod, as the
	// kubelet issues when it rotates a projected token.
	rotatedWorkloadToken = "rotated-workload-token"
)

// newAttestationClient is a fake API server knowing one workspace pod of
// alice's and two tokens bound to it.
func newAttestationClient() *fake.Clientset {
	client := fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "coder", Name: "coder-alice-dev", UID: "pod-uid",
		Labels: map[string]string{
			auth.DefaultAttestationUsernameKey:  "alice",
			auth.DefaultAttestationWorkspaceKey: "dev",
		},
		Annotations: map[string]string{auth.DefaultAttestationEmailKey: "alice@example.com"},
	}})

	client.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()
		if review.Spec.Token != workloadToken && review.Spec.Token != rotatedWorkloadToken {
			return true, review, nil
		}

		review.Status = authenticationv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{auth.ServiceAccountAudience},
			User: authenticationv1.UserInfo{
				Username: "system:serviceaccount:coder:workspace",
				Extra: map[string]authenticationv1.ExtraValue{
					"authentication.kubernetes.io/pod-name": {"coder-alice-dev"},
					"authentication.kubernetes.io/pod-uid":  {"pod-uid"},
				},
			},
		}

		return true, review, nil
	})

	return client
}

func TestAttestedMCPSessionBinding(t *testing.T) {
	fixture := newMCPFixtureWithAuth(t, &auth.Options{Attestation: &auth.AttestationOptions{
		Client:      newAttestationClient(),
		Namespaces:  []string{"coder"},
		MatchBy:     auth.MatchByEmail,
		WorkspaceID: "coder/{username}/{workspace}",
	}})

	t.Run("an attested session is bound to the verified workspace", func(t *testing.T) {
		client := alice()
		client.session = "attested"
		client.attestation = workloadToken
		client.workspace = "ignored"
		fixture.connect(t, client)

		row := fixture.sessionRow(t, "alice", "attested")
		assert.Equal(t, "coder/alice/dev", row.Workspace)
		assert.Equal(t, auth.AttestationAttested, row.Attestation)
	})

	t.Run("an attested session keeps working when its attestation token rotates", func(t *testing.T) {
		client := alice()
		client.session = "rotated"
		client.attestation = workloadToken

		response := fixture.post(t, client, "", initializeMessage())
		require.Equal(t, http.StatusOK, response.StatusCode)

		mcpSessionID := response.Header.Get("Mcp-Session-Id")
		require.NotEmpty(t, mcpSessionID)

		client.attestation = rotatedWorkloadToken
		ping := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"}
		assert.Equal(t, http.StatusOK, fixture.post(t, client, mcpSessionID, ping).StatusCode)
	})

	t.Run("another principal or session on an attested MCP session gets 404", func(t *testing.T) {
		client := alice()
		client.session = "attested-owner"
		client.attestation = workloadToken

		response := fixture.post(t, client, "", initializeMessage())
		require.Equal(t, http.StatusOK, response.StatusCode)

		mcpSessionID := response.Header.Get("Mcp-Session-Id")
		require.NotEmpty(t, mcpSessionID)

		ping := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"}

		other := bob()
		other.session = client.session
		assert.Equal(t, http.StatusNotFound, fixture.post(t, other, mcpSessionID, ping).StatusCode, "another principal")

		otherSession := client
		otherSession.session = "attested-other"
		assert.Equal(t, http.StatusNotFound, fixture.post(t, otherSession, mcpSessionID, ping).StatusCode, "another session")
	})

	t.Run("a session started without an attestation that later sends one gets 404, then 400", func(t *testing.T) {
		client := alice()
		client.session = "unattested"

		response := fixture.post(t, client, "", initializeMessage())
		require.Equal(t, http.StatusOK, response.StatusCode)

		mcpSessionID := response.Header.Get("Mcp-Session-Id")
		require.NotEmpty(t, mcpSessionID)

		client.attestation = workloadToken
		listTools := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}
		assert.Equal(t, http.StatusNotFound, fixture.post(t, client, mcpSessionID, listTools).StatusCode)

		assert.Equal(t, http.StatusBadRequest, fixture.post(t, client, "", initializeMessage()).StatusCode)
		assert.Equal(t, "claimed/default", fixture.sessionRow(t, "alice", "unattested").Workspace)
	})
}
