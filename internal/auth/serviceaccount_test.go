package auth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/solidDoWant/toolgate/internal/auth"
	tgtestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

const kubernetesIssuer = "https://kubernetes.default.svc.cluster.local"

// podToken is what the fake API server knows about one token it issued.
type podToken struct {
	namespace, serviceAccount string
	podName, podUID           string
	audiences                 []string
}

// fakeAPIServer answers TokenReviews the way the Kubernetes API server does:
// a token is authenticated only if it carries one of the requested audiences,
// and the status lists the audiences that matched.
type fakeAPIServer struct {
	client  *fake.Clientset
	signer  jose.Signer
	mutex   sync.Mutex
	tokens  map[string]podToken
	reviews atomic.Int32
	// ignoreAudiences answers like an authenticator that doesn't support
	// audiences: any known token is authenticated and no audience is listed.
	ignoreAudiences bool
}

func newFakeAPIServer(t *testing.T, objects ...runtime.Object) *fakeAPIServer {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, nil)
	require.NoError(t, err)

	server := &fakeAPIServer{client: fake.NewClientset(objects...), signer: signer, tokens: map[string]podToken{}}

	server.client.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		server.reviews.Add(1)

		review := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()

		server.mutex.Lock()
		issued, ok := server.tokens[review.Spec.Token]
		server.mutex.Unlock()

		// Without requested audiences, the API server checks its own.
		requested := review.Spec.Audiences
		if len(requested) == 0 {
			requested = []string{"https://kubernetes.default.svc"}
		}

		var matched []string

		for _, audience := range requested {
			if slices.Contains(issued.audiences, audience) {
				matched = append(matched, audience)
			}
		}

		if server.ignoreAudiences {
			matched = nil
		}

		if !ok || (len(matched) == 0 && !server.ignoreAudiences) {
			review.Status = authenticationv1.TokenReviewStatus{Error: "invalid bearer token"}

			return true, review, nil
		}

		review.Status = authenticationv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     matched,
			User: authenticationv1.UserInfo{
				Username: "system:serviceaccount:" + issued.namespace + ":" + issued.serviceAccount,
				Extra: map[string]authenticationv1.ExtraValue{
					"authentication.kubernetes.io/pod-name": {issued.podName},
					"authentication.kubernetes.io/pod-uid":  {issued.podUID},
				},
			},
		}

		return true, review, nil
	})

	return server
}

// issue mints a projected token for the pod's service account.
func (s *fakeAPIServer) issue(t *testing.T, issued podToken) string {
	t.Helper()

	now := time.Now()
	token, err := jwt.Signed(s.signer).Claims(jwt.Claims{
		Issuer:   kubernetesIssuer,
		Subject:  "system:serviceaccount:" + issued.namespace + ":" + issued.serviceAccount,
		Audience: issued.audiences,
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
	}).Serialize()
	require.NoError(t, err)

	s.mutex.Lock()
	s.tokens[token] = issued
	s.mutex.Unlock()

	return token
}

func controller(apiVersion, kind, name string, uid types.UID) []metav1.OwnerReference {
	isController := true

	return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: &isController}}
}

func newPodFixture(t *testing.T, namespaces []string, objects ...runtime.Object) (fixture, *fakeAPIServer) {
	t.Helper()

	server := newFakeAPIServer(t, objects...)
	f := newFixture(t, auth.Options{ServiceAccounts: &auth.ServiceAccountOptions{
		Client:     server.client,
		Namespaces: namespaces,
	}}, false)

	return f, server
}

func decodeIdentity(t *testing.T, body []byte) auth.Identity {
	t.Helper()

	var identity auth.Identity
	require.NoError(t, json.Unmarshal(body, &identity))

	return identity
}

func TestAC1_04PodSignIn(t *testing.T) {
	t.Parallel()

	runnerPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace:       "ci",
		Name:            "runner-abc",
		UID:             "pod-uid-1",
		OwnerReferences: controller("batch/v1", "Job", "runner", "job-uid"),
	}}
	runner := podToken{namespace: "ci", serviceAccount: "runner", podName: "runner-abc", podUID: "pod-uid-1", audiences: []string{"toolgate"}}

	t.Run("[AC-1-04] pod in an allowed namespace is attested", func(t *testing.T) {
		t.Parallel()

		f, server := newPodFixture(t, []string{"ci"}, runnerPod)

		response := f.do(t, server.issue(t, runner), map[string]string{"X-Toolgate-Session": "s-1", "X-Toolgate-Workspace": "elsewhere"})
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())

		identity := decodeIdentity(t, response.Body.Bytes())
		assert.Equal(t, "system:serviceaccount:ci:runner", identity.Principal)
		assert.Equal(t, auth.AttestationAttested, identity.Attestation)
		assert.Equal(t, "k8s/ci/Job/runner", identity.Workspace)
		assert.Equal(t, "s-1", identity.Session)
	})

	t.Run("[AC-1-04] pod in a namespace that isn't allowed gets 401", func(t *testing.T) {
		t.Parallel()

		f, server := newPodFixture(t, []string{"other"}, runnerPod)

		response := f.do(t, server.issue(t, runner), nil)
		assert.Equal(t, http.StatusUnauthorized, response.Code)

		refusals := f.recorder.all()
		require.Len(t, refusals, 1)
		assert.Equal(t, "namespace not allowed", refusals[0].Reason)
	})

	t.Run("[AC-1-04] token with another audience gets 401", func(t *testing.T) {
		t.Parallel()

		f, server := newPodFixture(t, []string{"ci"}, runnerPod)

		other := runner
		other.audiences = []string{"https://kubernetes.default.svc"}

		response := f.do(t, server.issue(t, other), nil)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
	})
}

func TestPodSignInRequiresAudienceInReview(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "debug", UID: "pod-uid-3"}}
	f, server := newPodFixture(t, []string{"ci"}, pod)
	server.ignoreAudiences = true

	token := server.issue(t, podToken{namespace: "ci", serviceAccount: "default", podName: "debug", podUID: "pod-uid-3", audiences: []string{"toolgate"}})
	assert.Equal(t, http.StatusUnauthorized, f.do(t, token, nil).Code)
}

func TestPodSignInReportsOwner(t *testing.T) {
	t.Parallel()

	deploymentPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ci", Name: "web-7d9-xyz", UID: "pod-uid-2",
		OwnerReferences: controller("apps/v1", "ReplicaSet", "web-7d9", "rs-uid"),
	}}
	replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ci", Name: "web-7d9", UID: "rs-uid",
		OwnerReferences: controller("apps/v1", "Deployment", "web", "deploy-uid"),
	}}
	barePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "debug", UID: "pod-uid-3"}}

	tests := []struct {
		name          string
		token         podToken
		wantStatus    int
		wantWorkspace string
	}{
		{
			name:          "replicaset reports its deployment",
			token:         podToken{namespace: "ci", serviceAccount: "web", podName: "web-7d9-xyz", podUID: "pod-uid-2", audiences: []string{"toolgate"}},
			wantStatus:    http.StatusOK,
			wantWorkspace: "k8s/ci/Deployment/web",
		},
		{
			name:          "pod without a controller reports itself",
			token:         podToken{namespace: "ci", serviceAccount: "default", podName: "debug", podUID: "pod-uid-3", audiences: []string{"toolgate"}},
			wantStatus:    http.StatusOK,
			wantWorkspace: "k8s/ci/Pod/debug",
		},
		{
			name:       "pod with another UID is refused",
			token:      podToken{namespace: "ci", serviceAccount: "default", podName: "debug", podUID: "old-uid", audiences: []string{"toolgate"}},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f, server := newPodFixture(t, []string{"ci"}, deploymentPod, replicaSet, barePod)

			response := f.do(t, server.issue(t, test.token), nil)
			require.Equal(t, test.wantStatus, response.Code, response.Body.String())

			if test.wantStatus == http.StatusOK {
				assert.Equal(t, test.wantWorkspace, decodeIdentity(t, response.Body.Bytes()).Workspace)
			}
		})
	}
}

func TestPodSignInCachesTokenReview(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "debug", UID: "pod-uid-3"}}
	f, server := newPodFixture(t, []string{"ci"}, pod)

	token := server.issue(t, podToken{namespace: "ci", serviceAccount: "default", podName: "debug", podUID: "pod-uid-3", audiences: []string{"toolgate"}})

	for range 3 {
		require.Equal(t, http.StatusOK, f.do(t, token, nil).Code)
	}

	assert.Equal(t, int32(1), server.reviews.Load())
}

func TestUserTokensStillWorkWithPodSignIn(t *testing.T) {
	t.Parallel()

	f, server := newPodFixture(t, []string{"ci"})

	response := f.do(t, f.provider.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice"}), nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	identity := decodeIdentity(t, response.Body.Bytes())
	assert.Equal(t, "alice", identity.Principal)
	assert.Equal(t, auth.AttestationUser, identity.Attestation)
	assert.Equal(t, int32(0), server.reviews.Load())
}
