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

const workloadToken = "workload-token"

// newAttestationClient is a fake API server knowing one workspace pod of
// alice's and one token bound to it.
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
		if review.Spec.Token != workloadToken {
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
