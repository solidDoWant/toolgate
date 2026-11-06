package attestation

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	launcherPod = "virt-launcher-vm-1-abcde"
	launcherUID = "7a1f0c52-0000-4000-8000-000000000001"
)

// fakeAPIServer holds a virt-launcher pod owned by a VirtualMachineInstance
// and answers TokenRequest like the real API server: it refuses a bound pod
// that doesn't exist or has another UID.
type fakeAPIServer struct {
	mu       sync.Mutex
	pod      corev1.Pod
	requests []authenticationv1.TokenRequest
	auth     []string
	paths    []string
}

func (f *fakeAPIServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var tokenRequest authenticationv1.TokenRequest
	if err := json.NewDecoder(request.Body).Decode(&tokenRequest); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)

		return
	}

	f.mu.Lock()
	f.requests = append(f.requests, tokenRequest)
	f.auth = append(f.auth, request.Header.Get("Authorization"))
	f.paths = append(f.paths, request.URL.Path)
	pod := f.pod
	f.mu.Unlock()

	writer.Header().Set("Content-Type", "application/json")

	ref := tokenRequest.Spec.BoundObjectRef
	if ref == nil || ref.Name != pod.Name || string(ref.UID) != string(pod.UID) {
		writer.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(writer).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure,
			Message:  "the bound object does not exist",
			Reason:   metav1.StatusReasonInvalid,
			Code:     http.StatusUnprocessableEntity,
		})

		return
	}

	tokenRequest.Status.Token = "minted-token"
	tokenRequest.TypeMeta = metav1.TypeMeta{Kind: "TokenRequest", APIVersion: "authentication.k8s.io/v1"}

	writer.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(writer).Encode(tokenRequest)
}

func newFakeAPIServer(t *testing.T) (*fakeAPIServer, *httptest.Server) {
	t.Helper()

	controller := true
	fake := &fakeAPIServer{pod: corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: launcherPod, Namespace: "vms", UID: launcherUID,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: "vm-1", Controller: &controller,
			}},
		},
	}}
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)

	return fake, server
}

// serviceAccountDisk writes a KubeVirt serviceAccount disk for the pod.
func serviceAccountDisk(t *testing.T, server *httptest.Server, podName, podUID string) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	require.NoError(t, err)

	claims := map[string]any{
		"kubernetes.io": map[string]any{
			"namespace":      "vms",
			"serviceaccount": map[string]any{"name": "vm-1", "uid": "sa-uid"},
			"pod":            map[string]any{"name": podName, "uid": podUID},
		},
	}
	token, err := jwt.Signed(signer).Claims(jwt.Claims{Subject: "system:serviceaccount:vms:vm-1", Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}).Claims(claims).Serialize()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "namespace"), []byte("vms\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))

	return dir
}

func TestTokenRequestBindsTheTokenToTheLauncherPod(t *testing.T) {
	fake, server := newFakeAPIServer(t)
	dir := serviceAccountDisk(t, server, launcherPod, launcherUID)

	source := Source{TokenRequest: dir, KubeAPI: server.URL}

	token, err := source.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "minted-token", token)

	// Never cached: a second run asks again.
	_, err = source.Token(t.Context())
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	require.Len(t, fake.requests, 2)
	assert.Equal(t, "/api/v1/namespaces/vms/serviceaccounts/vm-1/token", fake.paths[0])
	assert.Contains(t, fake.auth[0], "Bearer ey", "authenticates with the mounted token")

	spec := fake.requests[0].Spec
	assert.Equal(t, []string{"toolgate"}, spec.Audiences)
	require.NotNil(t, spec.ExpirationSeconds)
	assert.EqualValues(t, 600, *spec.ExpirationSeconds)
	require.NotNil(t, spec.BoundObjectRef)
	assert.Equal(t, "Pod", spec.BoundObjectRef.Kind)
	assert.Equal(t, launcherPod, spec.BoundObjectRef.Name)
	assert.EqualValues(t, launcherUID, spec.BoundObjectRef.UID)
}

func TestTokenRequestRefusalShowsTheAPIServersMessage(t *testing.T) {
	_, server := newFakeAPIServer(t)
	dir := serviceAccountDisk(t, server, launcherPod, "another-uid")

	token, err := Source{TokenRequest: dir, KubeAPI: server.URL}.Token(t.Context())
	require.Error(t, err)
	assert.Empty(t, token)
	assert.Contains(t, err.Error(), "the bound object does not exist")
	assert.NotContains(t, err.Error(), "ey", "no token in the message")
}

func TestTokenRequestFailures(t *testing.T) {
	_, server := newFakeAPIServer(t)

	tests := []struct {
		name  string
		setup func(dir string)
		want  string
	}{
		{"no namespace file", func(dir string) { require.NoError(t, os.Remove(filepath.Join(dir, "namespace"))) }, "namespace"},
		{"token is not a JWT", func(dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "token"), []byte("nope"), 0o600))
		}, "not a JWT"},
		{"no CA file", func(dir string) { require.NoError(t, os.Remove(filepath.Join(dir, "ca.crt"))) }, "ca.crt"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := serviceAccountDisk(t, server, launcherPod, launcherUID)
			test.setup(dir)

			_, err := Source{TokenRequest: dir, KubeAPI: server.URL}.Token(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestOnlyOneSource(t *testing.T) {
	err := Source{TokenFile: "a", TokenRequest: "b"}.Validate()
	require.EqualError(t, err, "Set only one of TOOLGATE_ATTESTATION_TOKEN_FILE, TOOLGATE_ATTESTATION_SPIFFE_SOCKET and TOOLGATE_ATTESTATION_TOKEN_REQUEST.")
	require.NoError(t, Source{TokenFile: "a"}.Validate())
	require.NoError(t, Source{}.Validate())

	token, err := Source{}.Token(t.Context())
	require.NoError(t, err)
	assert.Empty(t, token)
}
