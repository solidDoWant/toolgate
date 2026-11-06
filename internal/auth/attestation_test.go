package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/solidDoWant/toolgate/internal/auth"
	tgtestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

// workspacePod is a Coder workspace pod of alice's: the email is an
// annotation, because label values can't hold @.
func workspacePod(labels map[string]string) *corev1.Pod {
	merged := map[string]string{
		auth.DefaultAttestationUserIDKey:    "user-1",
		auth.DefaultAttestationUsernameKey:  "alice",
		auth.DefaultAttestationWorkspaceKey: "dev",
	}

	for key, value := range labels {
		if value == "" {
			delete(merged, key)
		} else {
			merged[key] = value
		}
	}

	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "coder",
		Name:        "coder-alice-dev",
		UID:         "pod-uid-ws",
		Labels:      merged,
		Annotations: map[string]string{auth.DefaultAttestationEmailKey: "alice@example.com"},
	}}
}

var workspaceToken = podToken{
	namespace: "coder", serviceAccount: "workspace", podName: "coder-alice-dev", podUID: "pod-uid-ws",
	audiences: []string{"toolgate"},
}

func emailAttestation(server *fakeAPIServer) *auth.AttestationOptions {
	return &auth.AttestationOptions{
		Client:      server.client,
		Namespaces:  []string{"coder"},
		MatchBy:     auth.MatchByEmail,
		WorkspaceID: "coder/{username}/{workspace}",
	}
}

func claimAttestation(server *fakeAPIServer) *auth.AttestationOptions {
	options := emailAttestation(server)
	options.MatchBy = auth.MatchByClaim
	options.MatchClaim = "preferred_username"
	options.MatchKey = auth.AttestationKeyUsername

	return options
}

func newAttestationFixture(t *testing.T, options func(*fakeAPIServer) *auth.AttestationOptions, objects ...runtime.Object) (fixture, *fakeAPIServer) {
	t.Helper()

	server := newFakeAPIServer(t, objects...)
	f := newFixture(t, auth.Options{Attestation: options(server)}, false)

	return f, server
}

var aliceClaims = tgtestutil.TokenClaims{
	Subject: "alice-sub", Email: "Alice@Example.com", EmailVerified: true,
	Extra: map[string]any{"preferred_username": "alice"},
}

func TestAttestedUserSession(t *testing.T) {
	t.Parallel()

	f, server := newAttestationFixture(t, emailAttestation, workspacePod(nil))

	response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{
		auth.AttestationHeader: server.issue(t, workspaceToken),
		auth.WorkspaceHeader:   "elsewhere",
		auth.SessionHeader:     "s-1",
	})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	identity := decodeIdentity(t, response.Body.Bytes())
	assert.Equal(t, "alice-sub", identity.Principal)
	assert.Equal(t, auth.KindUser, identity.Kind)
	assert.Equal(t, auth.AttestationAttested, identity.Attestation)
	assert.Equal(t, "coder/alice/dev", identity.Workspace, "the workspace header can't change a verified workspace")
	assert.Equal(t, "s-1", identity.Session)
	assert.Empty(t, f.recorder.all())
}

func TestAttestationMatchByClaim(t *testing.T) {
	t.Parallel()

	f, server := newAttestationFixture(t, func(server *fakeAPIServer) *auth.AttestationOptions {
		options := claimAttestation(server)
		options.MatchClaim = "uid"
		options.MatchKey = auth.AttestationKeyUserID
		options.WorkspaceID = "coder/{userID}/{workspace}"

		return options
	}, workspacePod(nil))

	claims := aliceClaims
	claims.Extra = map[string]any{"uid": "user-1"}

	response := f.do(t, f.provider.MintAccessToken(t, claims), map[string]string{auth.AttestationHeader: server.issue(t, workspaceToken)})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	identity := decodeIdentity(t, response.Body.Bytes())
	assert.Equal(t, auth.AttestationAttested, identity.Attestation)
	assert.Equal(t, "coder/user-1/dev", identity.Workspace)
}

func TestAttestationOfAVirtLauncherPodReadsNoVMI(t *testing.T) {
	t.Parallel()

	pod := workspacePod(nil)
	pod.Name = "virt-launcher-dev-abcde"
	pod.OwnerReferences = controller("kubevirt.io/v1", "VirtualMachineInstance", "dev", "vmi-uid")

	f, server := newAttestationFixture(t, emailAttestation, pod)

	token := workspaceToken
	token.podName = pod.Name

	response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{auth.AttestationHeader: server.issue(t, token)})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "coder/alice/dev", decodeIdentity(t, response.Body.Bytes()).Workspace)

	for _, action := range server.client.Actions() {
		assert.Contains(t, []string{"tokenreviews", "pods"}, action.GetResource().Resource,
			"the gateway reads only the token review and the pod")
	}
}

// TestAttestationRefusals checks each failed verdict: the request is served
// as the user with the claimed workspace, and auth.refuse names the reason
// and the user's principal.
func TestAttestationRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options func(*fakeAPIServer) *auth.AttestationOptions
		pod     *corev1.Pod
		claims  tgtestutil.TokenClaims
		// header builds the attestation header values.
		header func(t *testing.T, server *fakeAPIServer) []string
		reason string
	}{
		{
			name: "pod labelled for another user", options: emailAttestation,
			pod:    workspacePod(nil),
			claims: tgtestutil.TokenClaims{Subject: "bob-sub", Email: "bob@example.com", EmailVerified: true},
			reason: "attestation owner mismatch",
		},
		{
			name: "unverified email never matches", options: emailAttestation,
			pod:    workspacePod(nil),
			claims: tgtestutil.TokenClaims{Subject: "alice-sub", Email: "alice@example.com"},
			reason: "attestation owner mismatch",
		},
		{
			name: "invalid token", options: emailAttestation, pod: workspacePod(nil),
			header: func(*testing.T, *fakeAPIServer) []string { return []string{"not-a-token"} },
			reason: "attestation token invalid",
		},
		{
			name: "token with another audience", options: emailAttestation, pod: workspacePod(nil),
			header: func(t *testing.T, server *fakeAPIServer) []string {
				token := workspaceToken
				token.audiences = []string{"https://kubernetes.default.svc"}

				return []string{server.issue(t, token)}
			},
			reason: "attestation token invalid",
		},
		{
			name: "namespace not listed", options: emailAttestation, pod: workspacePod(nil),
			header: func(t *testing.T, server *fakeAPIServer) []string {
				token := workspaceToken
				token.namespace = "other"

				return []string{server.issue(t, token)}
			},
			reason: "attestation namespace not allowed",
		},
		{
			name: "not bound to a pod", options: emailAttestation, pod: workspacePod(nil),
			header: func(t *testing.T, server *fakeAPIServer) []string {
				token := workspaceToken
				token.podName = ""

				return []string{server.issue(t, token)}
			},
			reason: "attestation not bound to a pod",
		},
		{
			name: "pod not found", options: emailAttestation, pod: workspacePod(nil),
			header: func(t *testing.T, server *fakeAPIServer) []string {
				token := workspaceToken
				token.podName = "gone"

				return []string{server.issue(t, token)}
			},
			reason: "attestation pod lookup failed",
		},
		{
			name: "pod UID differs", options: emailAttestation, pod: workspacePod(nil),
			header: func(t *testing.T, server *fakeAPIServer) []string {
				token := workspaceToken
				token.podUID = "old-uid"

				return []string{server.issue(t, token)}
			},
			reason: "attestation pod lookup failed",
		},
		{
			name: "owner key missing", options: emailAttestation,
			pod: func() *corev1.Pod {
				pod := workspacePod(nil)
				pod.Annotations = nil

				return pod
			}(),
			reason: "attestation owner key missing",
		},
		{
			name: "match by claim without the claim", options: claimAttestation, pod: workspacePod(nil),
			claims: tgtestutil.TokenClaims{Subject: "alice-sub"},
			reason: "attestation owner claim missing",
		},
		{
			name: "match by claim with a claim that isn't a string", options: claimAttestation, pod: workspacePod(nil),
			claims: tgtestutil.TokenClaims{Subject: "alice-sub", Extra: map[string]any{"preferred_username": []string{"alice"}}},
			reason: "attestation owner claim missing",
		},
		{
			name: "match by claim with another value", options: claimAttestation, pod: workspacePod(nil),
			claims: tgtestutil.TokenClaims{Subject: "alice-sub", Extra: map[string]any{"preferred_username": "bob"}},
			reason: "attestation owner mismatch",
		},
		{
			name: "workspace key missing", options: emailAttestation,
			pod:    workspacePod(map[string]string{auth.DefaultAttestationWorkspaceKey: ""}),
			reason: "attestation workspace key invalid",
		},
		{
			name: "workspace key adds a path segment", options: emailAttestation,
			pod: func() *corev1.Pod {
				pod := workspacePod(map[string]string{auth.DefaultAttestationWorkspaceKey: ""})
				pod.Annotations[auth.DefaultAttestationWorkspaceKey] = "dev/../other"

				return pod
			}(),
			reason: "attestation workspace key invalid",
		},
		{
			name: "workspace key of 64 characters", options: emailAttestation,
			pod:    workspacePod(map[string]string{auth.DefaultAttestationWorkspaceKey: strings.Repeat("w", 64)}),
			reason: "attestation workspace key invalid",
		},
		{
			name: "header repeated", options: emailAttestation, pod: workspacePod(nil),
			header: func(t *testing.T, server *fakeAPIServer) []string {
				token := server.issue(t, workspaceToken)

				return []string{token, token}
			},
			reason: "attestation header repeated or too large",
		},
		{
			name: "header over 16 KiB", options: emailAttestation, pod: workspacePod(nil),
			header: func(*testing.T, *fakeAPIServer) []string {
				return []string{strings.Repeat("a", auth.MaxAttestationBytes+1)}
			},
			reason: "attestation header repeated or too large",
		},
		{
			name: "header of exactly 16 KiB is checked as a token", options: emailAttestation, pod: workspacePod(nil),
			header: func(*testing.T, *fakeAPIServer) []string {
				return []string{strings.Repeat("a", auth.MaxAttestationBytes)}
			},
			reason: "attestation token invalid",
		},
		{
			name: "SVID without spiffeIDPattern", options: emailAttestation, pod: workspacePod(nil),
			header: func(t *testing.T, _ *fakeAPIServer) []string {
				return []string{newSigningKey(t, "key-1").sign(t, svidClaims{subject: "spiffe://allowed.test/coder/alice/dev"})}
			},
			reason: "attestation SPIFFE ID doesn't match the pattern",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f, server := newAttestationFixture(t, test.options, test.pod)

			claims := test.claims
			if claims.Subject == "" {
				claims = aliceClaims
			}

			header := test.header
			if header == nil {
				header = func(t *testing.T, server *fakeAPIServer) []string { return []string{server.issue(t, workspaceToken)} }
			}

			request := newRequest(t, f.provider.MintAccessToken(t, claims))
			request.Header.Set(auth.WorkspaceHeader, "repo")

			for _, value := range header(t, server) {
				request.Header.Add(auth.AttestationHeader, value)
			}

			response := f.serve(request)
			require.Equal(t, http.StatusOK, response.Code, "a failed attestation never refuses the request: %s", response.Body.String())

			identity := decodeIdentity(t, response.Body.Bytes())
			assert.Equal(t, auth.AttestationUser, identity.Attestation)
			assert.Equal(t, "claimed/repo", identity.Workspace)
			assert.Equal(t, claims.Subject, identity.Principal)

			refusals := f.recorder.all()
			require.Len(t, refusals, 1)
			assert.Equal(t, test.reason, refusals[0].Reason)
			assert.Equal(t, claims.Subject, refusals[0].Principal)
		})
	}
}

func TestWorkspaceKeyOf63CharactersIsAccepted(t *testing.T) {
	t.Parallel()

	name := strings.Repeat("w", 63)
	f, server := newAttestationFixture(t, emailAttestation, workspacePod(map[string]string{auth.DefaultAttestationWorkspaceKey: name}))

	response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{auth.AttestationHeader: server.issue(t, workspaceToken)})
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "coder/alice/"+name, decodeIdentity(t, response.Body.Bytes()).Workspace)
}

func TestAttestationNeverAuthenticates(t *testing.T) {
	t.Parallel()

	f, server := newAttestationFixture(t, emailAttestation, workspacePod(nil))

	response := f.do(t, "not-a-user-token", map[string]string{auth.AttestationHeader: server.issue(t, workspaceToken)})
	assert.Equal(t, http.StatusUnauthorized, response.Code)

	refusals := f.recorder.all()
	require.Len(t, refusals, 1)
	assert.Equal(t, "invalid token", refusals[0].Reason)
	assert.Empty(t, refusals[0].Principal)
	assert.Zero(t, server.reviews.Load(), "the attestation isn't checked for a bad user token")
}

func TestWorkloadBearerIgnoresTheAttestationHeader(t *testing.T) {
	t.Parallel()

	runnerPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "runner", UID: "pod-uid-r"}}
	server := newFakeAPIServer(t, runnerPod, workspacePod(nil))
	f := newFixture(t, auth.Options{
		ServiceAccounts: &auth.ServiceAccountOptions{Client: server.client, Namespaces: []string{"ci"}},
		Attestation:     emailAttestation(server),
	}, false)

	runner := podToken{namespace: "ci", serviceAccount: "runner", podName: "runner", podUID: "pod-uid-r", audiences: []string{"toolgate"}}

	response := f.do(t, server.issue(t, runner), map[string]string{auth.AttestationHeader: server.issue(t, workspaceToken)})
	require.Equal(t, http.StatusOK, response.Code)

	identity := decodeIdentity(t, response.Body.Bytes())
	assert.Equal(t, auth.KindServiceAccount, identity.Kind)
	assert.Equal(t, "k8s/ci/Pod/runner", identity.Workspace)
	assert.Equal(t, int32(1), server.reviews.Load(), "only the bearer token is reviewed")
	assert.Empty(t, f.recorder.all())
}

func TestAttestationHeaderIgnoredWithoutConfiguration(t *testing.T) {
	t.Parallel()

	f := newFixture(t, auth.Options{}, false)

	response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{auth.AttestationHeader: "anything"})
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, auth.AttestationUser, decodeIdentity(t, response.Body.Bytes()).Attestation)
	assert.Empty(t, f.recorder.all())
}

func TestUnavailableAttestationCheckGets503(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		verb     string
		resource string
		err      error
	}{
		{name: "TokenReview transport error", verb: "create", resource: "tokenreviews", err: errors.New("connection refused")},
		{name: "TokenReview 5xx", verb: "create", resource: "tokenreviews", err: apierrors.NewInternalError(errors.New("etcd"))},
		{name: "pod GET transport error", verb: "get", resource: "pods", err: errors.New("connection reset")},
		{name: "pod GET 5xx", verb: "get", resource: "pods", err: apierrors.NewServiceUnavailable("overloaded")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f, server := newAttestationFixture(t, emailAttestation, workspacePod(nil))
			token := server.issue(t, workspaceToken)

			server.client.PrependReactor(test.verb, test.resource, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, test.err
			})

			response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{auth.AttestationHeader: token})
			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.Equal(t, "1", response.Header().Get("Retry-After"))
			assert.Empty(t, f.recorder.all(), "an unavailable check writes no auth.refuse")
		})
	}

	t.Run("pod GET 403 is a verdict", func(t *testing.T) {
		t.Parallel()

		f, server := newAttestationFixture(t, emailAttestation, workspacePod(nil))
		token := server.issue(t, workspaceToken)

		server.client.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "coder-alice-dev", errors.New("no"))
		})

		response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{auth.AttestationHeader: token})
		assert.Equal(t, http.StatusOK, response.Code)

		refusals := f.recorder.all()
		require.Len(t, refusals, 1)
		assert.Equal(t, "attestation pod lookup failed", refusals[0].Reason)
	})
}

func TestAttestationPodFactsAreCached(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Now()}
	f, server := newAttestationFixture(t, func(server *fakeAPIServer) *auth.AttestationOptions {
		options := emailAttestation(server)
		options.Now = clock.Now

		return options
	}, workspacePod(nil))

	token := server.issue(t, workspaceToken)
	headers := map[string]string{auth.AttestationHeader: token}

	attestedWorkspace := func(claims tgtestutil.TokenClaims) string {
		response := f.do(t, f.provider.MintAccessToken(t, claims), headers)
		require.Equal(t, http.StatusOK, response.Code)

		return decodeIdentity(t, response.Body.Bytes()).Workspace
	}

	require.Equal(t, "coder/alice/dev", attestedWorkspace(aliceClaims))

	// The pod is relabelled: the cached facts still decide.
	relabelled := workspacePod(map[string]string{auth.DefaultAttestationWorkspaceKey: "other"})
	_, err := server.client.CoreV1().Pods("coder").Update(t.Context(), relabelled, metav1.UpdateOptions{})
	require.NoError(t, err)

	clock.advance(59 * time.Minute)
	assert.Equal(t, "coder/alice/dev", attestedWorkspace(aliceClaims))
	assert.Equal(t, int32(1), server.reviews.Load())

	// The owner comparison runs on every request.
	bob := tgtestutil.TokenClaims{Subject: "bob-sub", Email: "bob@example.com", EmailVerified: true}
	assert.Equal(t, "claimed/default", attestedWorkspace(bob))
	assert.Equal(t, int32(1), server.reviews.Load())

	// After the token's expiry, at most 1 h, the new labels decide.
	clock.advance(2 * time.Minute)
	assert.Equal(t, "coder/alice/other", attestedWorkspace(aliceClaims))
	assert.Equal(t, int32(2), server.reviews.Load())
}

func TestAttestationWithJWTSVID(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	clock := &fakeClock{now: time.Now()}
	server := newFakeAPIServer(t)

	options := claimAttestation(server)
	options.SPIFFEIDPattern = "spiffe://allowed.test/coder/{username}/{workspace}"
	options.SPIFFETrustDomains = []string{"allowed.test"}

	f := newFixture(t, auth.Options{
		SPIFFE:      &auth.SPIFFEOptions{TrustDomains: []auth.SPIFFETrustDomain{endpoint.trustDomainConfig()}, Now: clock.Now},
		Attestation: options,
	}, false)

	// The bundle is fetched in the background.
	waitForBundle(t, f, key.sign(t, svidClaims{subject: "spiffe://allowed.test/ci"}))

	tests := []struct {
		name      string
		subject   string
		workspace string
		reason    string
	}{
		{name: "matching ID", subject: "spiffe://allowed.test/coder/alice/dev", workspace: "coder/alice/dev"},
		{name: "ID of another user", subject: "spiffe://allowed.test/coder/bob/dev", workspace: "claimed/default", reason: "attestation owner mismatch"},
		{name: "ID not matching the pattern", subject: "spiffe://allowed.test/other/alice/dev", workspace: "claimed/default", reason: "attestation SPIFFE ID doesn't match the pattern"},
		{name: "extra segment", subject: "spiffe://allowed.test/coder/alice/dev/x", workspace: "claimed/default", reason: "attestation SPIFFE ID doesn't match the pattern"},
		{name: "captured value of 64 characters", subject: "spiffe://allowed.test/coder/alice/" + strings.Repeat("w", 64), workspace: "claimed/default", reason: "attestation workspace key invalid"},
		{name: "captured value of 63 characters", subject: "spiffe://allowed.test/coder/alice/" + strings.Repeat("w", 63), workspace: "coder/alice/" + strings.Repeat("w", 63)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(f.recorder.all())

			response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{
				auth.AttestationHeader: key.sign(t, svidClaims{subject: test.subject}),
			})
			require.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, test.workspace, decodeIdentity(t, response.Body.Bytes()).Workspace)

			refusals := f.recorder.all()[before:]
			if test.reason == "" {
				assert.Empty(t, refusals)

				return
			}

			require.Len(t, refusals, 1)
			assert.Equal(t, test.reason, refusals[0].Reason)
		})
	}

	t.Run("expired SVID", func(t *testing.T) {
		before := len(f.recorder.all())

		response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{
			auth.AttestationHeader: key.sign(t, svidClaims{subject: "spiffe://allowed.test/coder/alice/dev", expiry: time.Now().Add(-time.Minute)}),
		})
		require.Equal(t, http.StatusOK, response.Code)

		refusals := f.recorder.all()[before:]
		require.Len(t, refusals, 1)
		assert.Equal(t, "attestation token invalid", refusals[0].Reason)
	})
}

func TestValidateAttestation(t *testing.T) {
	t.Parallel()

	valid := func() auth.AttestationOptions {
		return auth.AttestationOptions{
			Namespaces:  []string{"coder"},
			MatchBy:     auth.MatchByEmail,
			WorkspaceID: "coder/{username}/{workspace}",
		}
	}

	withSPIFFE := func(pattern string) auth.AttestationOptions {
		options := valid()
		options.MatchBy, options.MatchClaim, options.MatchKey = auth.MatchByClaim, "preferred_username", auth.AttestationKeyUsername
		options.SPIFFEIDPattern = pattern
		options.SPIFFETrustDomains = []string{"coder.example.com"}

		return options
	}

	tests := []struct {
		name    string
		options func() auth.AttestationOptions
		problem string
	}{
		{name: "valid", options: valid},
		{name: "valid with a pattern", options: func() auth.AttestationOptions {
			return withSPIFFE("spiffe://coder.example.com/coder/{username}/{workspace}")
		}},
		{name: "no namespaces", problem: "namespaces: required", options: func() auth.AttestationOptions {
			options := valid()
			options.Namespaces = nil

			return options
		}},
		{name: "bad namespace", problem: "not a namespace name", options: func() auth.AttestationOptions {
			options := valid()
			options.Namespaces = []string{"Coder"}

			return options
		}},
		{name: "bad match", problem: "match.by", options: func() auth.AttestationOptions {
			options := valid()
			options.MatchBy = ""

			return options
		}},
		{name: "claim without key", problem: "match.key", options: func() auth.AttestationOptions {
			options := valid()
			options.MatchBy, options.MatchClaim = auth.MatchByClaim, "sub"

			return options
		}},
		{name: "unknown placeholder", problem: "{email}", options: func() auth.AttestationOptions {
			options := valid()
			options.WorkspaceID = "coder/{email}"

			return options
		}},
		{name: "placeholder in the first segment", problem: "literal segment", options: func() auth.AttestationOptions {
			options := valid()
			options.WorkspaceID = "{username}/{workspace}"

			return options
		}},
		{name: "claimed first segment", problem: "literal segment", options: func() auth.AttestationOptions {
			options := valid()
			options.WorkspaceID = "claimed/{workspace}"

			return options
		}},
		{name: "k8s first segment", problem: "literal segment", options: func() auth.AttestationOptions {
			options := valid()
			options.WorkspaceID = "k8s/{workspace}"

			return options
		}},
		{name: "empty first segment", problem: "literal segment", options: func() auth.AttestationOptions {
			options := valid()
			options.WorkspaceID = "/{workspace}"

			return options
		}},
		{name: "no workspaceID", problem: "workspaceID: required", options: func() auth.AttestationOptions {
			options := valid()
			options.WorkspaceID = ""

			return options
		}},
		{name: "pattern with match by email", problem: "must be claim", options: func() auth.AttestationOptions {
			options := withSPIFFE("spiffe://coder.example.com/coder/{username}/{workspace}")
			options.MatchBy = auth.MatchByEmail

			return options
		}},
		{name: "pattern in another trust domain", problem: "not in spiffeTrustDomains", options: func() auth.AttestationOptions {
			return withSPIFFE("spiffe://other.example.com/coder/{username}/{workspace}")
		}},
		{name: "pattern without the match key", problem: "{username}, the match key", options: func() auth.AttestationOptions {
			options := withSPIFFE("spiffe://coder.example.com/coder/{userID}/{workspace}")
			options.WorkspaceID = "coder/{workspace}"

			return options
		}},
		{name: "pattern without a workspaceID placeholder", problem: "{workspace}, which workspaceID uses", options: func() auth.AttestationOptions {
			return withSPIFFE("spiffe://coder.example.com/coder/{username}")
		}},
		{name: "placeholder that isn't a whole segment", problem: "whole path segment", options: func() auth.AttestationOptions {
			return withSPIFFE("spiffe://coder.example.com/coder/{username}-{workspace}")
		}},
		{name: "pattern that isn't SPIFFE", problem: "spiffe://", options: func() auth.AttestationOptions {
			return withSPIFFE("https://coder.example.com/{username}/{workspace}")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			problems := auth.ValidateAttestation(test.options())
			if test.problem == "" {
				assert.Empty(t, problems)

				return
			}

			require.NotEmpty(t, problems)
			assert.Contains(t, errors.Join(problems...).Error(), test.problem)
		})
	}
}

func TestSPIFFEIDPatternQuotesLiterals(t *testing.T) {
	t.Parallel()

	key := newSigningKey(t, "key-1")
	endpoint := startBundleEndpoint(t, "allowed.test", key)
	server := newFakeAPIServer(t)

	options := claimAttestation(server)
	options.SPIFFEIDPattern = "spiffe://allowed.test/a.b/{username}/{workspace}"
	options.SPIFFETrustDomains = []string{"allowed.test"}

	f := newFixture(t, auth.Options{
		SPIFFE:      &auth.SPIFFEOptions{TrustDomains: []auth.SPIFFETrustDomain{endpoint.trustDomainConfig()}},
		Attestation: options,
	}, false)
	waitForBundle(t, f, key.sign(t, svidClaims{subject: "spiffe://allowed.test/ci"}))

	response := f.do(t, f.provider.MintAccessToken(t, aliceClaims), map[string]string{
		auth.AttestationHeader: key.sign(t, svidClaims{subject: "spiffe://allowed.test/axb/alice/dev"}),
	})
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "claimed/default", decodeIdentity(t, response.Body.Bytes()).Workspace, "the dot is literal")
}

// newRequest is the request fixture.do sends, for headers do can't set.
func newRequest(t *testing.T, token string) *http.Request {
	t.Helper()

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	request.RemoteAddr = "192.0.2.10:40000"
	request.Header.Set("Authorization", "Bearer "+token)

	return request
}

func (f fixture) serve(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)

	return response
}
