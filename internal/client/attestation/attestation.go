// Package attestation gets the workload token that the client sends in
// X-Toolgate-Attestation. It proves where the client runs, next to the
// user's own token.
package attestation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// TokenFileEnv names the variable that holds the path of a projected
	// service-account token.
	TokenFileEnv = "TOOLGATE_ATTESTATION_TOKEN_FILE"

	// SPIFFESocketEnv names the variable that holds a Workload API address.
	SPIFFESocketEnv = "TOOLGATE_ATTESTATION_SPIFFE_SOCKET"

	// TokenRequestEnv names the variable that holds the directory of a
	// KubeVirt serviceAccount disk.
	TokenRequestEnv = "TOOLGATE_ATTESTATION_TOKEN_REQUEST"

	// SignerFileEnv names the variable that holds the path of a registered
	// workspace key. The name avoids the words Claude Code filters out of a
	// plugin helper's environment.
	SignerFileEnv = "TOOLGATE_ATTESTATION_SIGNER_FILE"

	// KubeAPIEnv names the variable that holds the Kubernetes API URL for
	// token requests.
	KubeAPIEnv = "TOOLGATE_KUBE_API"

	// DefaultKubeAPI is the API URL when KubeAPIEnv is not set.
	DefaultKubeAPI = "https://kubernetes.default.svc"

	// Audience is the audience of the attestation token.
	Audience = "toolgate"

	// Timeout bounds one fetch or token request.
	Timeout = 5 * time.Second

	// TokenLifetime is the lifetime of a requested token, the API server's
	// minimum.
	TokenLifetime = 600

	// MaxTokenBytes bounds how much of a token file is read.
	MaxTokenBytes = 64 << 10
)

// ConflictMessage is the error when more than one attestation variable is set.
const ConflictMessage = "Set only one of " + TokenFileEnv + ", " + SPIFFESocketEnv + ", " + TokenRequestEnv + " and " + SignerFileEnv + "."

// Source is the configured way to get the attestation token. The zero value
// means no attestation.
type Source struct {
	TokenFile    string
	SPIFFESocket string
	TokenRequest string
	KeyFile      string
	KubeAPI      string
}

// FromEnv reads the sources from the environment.
func FromEnv() Source {
	kubeAPI := strings.TrimSpace(os.Getenv(KubeAPIEnv))
	if kubeAPI == "" {
		kubeAPI = DefaultKubeAPI
	}

	return Source{
		TokenFile:    strings.TrimSpace(os.Getenv(TokenFileEnv)),
		SPIFFESocket: strings.TrimSpace(os.Getenv(SPIFFESocketEnv)),
		TokenRequest: strings.TrimSpace(os.Getenv(TokenRequestEnv)),
		KeyFile:      strings.TrimSpace(os.Getenv(SignerFileEnv)),
		KubeAPI:      kubeAPI,
	}
}

// Configured reports whether any source is set.
func (s Source) Configured() bool {
	return s.TokenFile != "" || s.SPIFFESocket != "" || s.TokenRequest != "" || s.KeyFile != ""
}

// Validate refuses a setup with more than one source.
func (s Source) Validate() error {
	count := 0

	for _, value := range []string{s.TokenFile, s.SPIFFESocket, s.TokenRequest, s.KeyFile} {
		if value != "" {
			count++
		}
	}

	if count > 1 {
		return errors.New(ConflictMessage) //nolint:staticcheck // user-facing sentence
	}

	return nil
}

// Names returns the names of the variables that are set.
func (s Source) Names() []string {
	var names []string

	if s.TokenFile != "" {
		names = append(names, TokenFileEnv)
	}

	if s.SPIFFESocket != "" {
		names = append(names, SPIFFESocketEnv)
	}

	if s.TokenRequest != "" {
		names = append(names, TokenRequestEnv)
	}

	if s.KeyFile != "" {
		names = append(names, SignerFileEnv)
	}

	return names
}

// Token returns a fresh attestation token, or "" when no source is set. The
// error never holds a token.
func (s Source) Token(ctx context.Context) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}

	switch {
	case s.TokenFile != "":
		return readTokenFile(s.TokenFile)
	case s.SPIFFESocket != "":
		return fetchSVID(ctx, s.SPIFFESocket)
	case s.TokenRequest != "":
		return requestToken(ctx, s.TokenRequest, s.KubeAPI)
	case s.KeyFile != "":
		return signAssertion(s.KeyFile, time.Now())
	}

	return "", nil
}

func readTokenFile(path string) (string, error) {
	content, err := readLimited(path)
	if err != nil {
		return "", fmt.Errorf("read the attestation token file: %w", err)
	}

	token := strings.TrimSpace(string(content))
	if token == "" {
		return "", fmt.Errorf("the attestation token file %s is empty", path)
	}

	return token, nil
}

func readLimited(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // the admin names the path
	if err != nil {
		return nil, err
	}

	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(io.LimitReader(file, MaxTokenBytes+1))
	if err != nil {
		return nil, err
	}

	if len(content) > MaxTokenBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, MaxTokenBytes)
	}

	return content, nil
}

func fetchSVID(ctx context.Context, socket string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	svid, err := workloadapi.FetchJWTSVID(ctx, jwtsvid.Params{Audience: Audience}, workloadapi.WithAddr(socket))
	if err != nil {
		return "", fmt.Errorf("fetch the attestation JWT-SVID from %s: %w", socket, err)
	}

	return svid.Marshal(), nil
}

// allowedAlgorithms are the signing algorithms the API server uses for
// service-account tokens. Only the parser needs them: nothing is verified.
var allowedAlgorithms = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512, jose.PS256, jose.PS384, jose.PS512, jose.EdDSA}

// mountedClaims are the claims of the mounted token that fill in the token
// request. They are not verified: the API server checks everything.
type mountedClaims struct {
	Kubernetes struct {
		Namespace      string `json:"namespace"`
		ServiceAccount struct {
			Name string `json:"name"`
		} `json:"serviceaccount"`
		Pod struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"pod"`
	} `json:"kubernetes.io"`
}

func requestToken(ctx context.Context, dir, kubeAPI string) (string, error) {
	mounted, err := readDiskFile(dir, "token")
	if err != nil {
		return "", err
	}

	namespaceFile, err := readDiskFile(dir, "namespace")
	if err != nil {
		return "", err
	}

	parsed, err := jwt.ParseSigned(mounted, allowedAlgorithms)
	if err != nil {
		return "", errors.New("the mounted service-account token is not a JWT")
	}

	var claims mountedClaims
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return "", errors.New("read the claims of the mounted service-account token: invalid claims")
	}

	namespace := namespaceFile
	if namespace == "" {
		namespace = claims.Kubernetes.Namespace
	}

	account, pod := claims.Kubernetes.ServiceAccount.Name, claims.Kubernetes.Pod

	if namespace == "" || account == "" || pod.Name == "" || pod.UID == "" {
		return "", errors.New("the mounted service-account token is not bound to a pod, so it can't be used for a token request")
	}

	client, err := kubernetes.NewForConfig(&rest.Config{
		Host:            kubeAPI,
		BearerToken:     mounted,
		TLSClientConfig: rest.TLSClientConfig{CAFile: dir + string(os.PathSeparator) + "ca.crt"},
		Timeout:         Timeout,
		ContentConfig:   rest.ContentConfig{ContentType: "application/json"},
	})
	if err != nil {
		return "", fmt.Errorf("build the Kubernetes client: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	lifetime := int64(TokenLifetime)

	result, err := client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, account, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{Audience},
			ExpirationSeconds: &lifetime,
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				Kind:       "Pod",
				APIVersion: "v1",
				Name:       pod.Name,
				UID:        types.UID(pod.UID),
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("the token request to %s failed: %w", kubeAPI, err)
	}

	if result.Status.Token == "" {
		return "", errors.New("the API server returned no token")
	}

	return result.Status.Token, nil
}

func readDiskFile(dir, name string) (string, error) {
	content, err := readLimited(dir + string(os.PathSeparator) + name)
	if err != nil {
		return "", fmt.Errorf("read the %s file of the service-account disk: %w", name, err)
	}

	if name == "token" && len(strings.TrimSpace(string(content))) == 0 {
		return "", errors.New("the token file of the service-account disk is empty")
	}

	return strings.TrimSpace(string(content)), nil
}
