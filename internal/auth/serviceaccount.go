package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ServiceAccountAudience is the audience a projected service-account token
// must carry for the gateway (SPEC.md §6).
const ServiceAccountAudience = "toolgate"

const (
	serviceAccountPrefix = "system:serviceaccount:"
	podNameExtra         = "authentication.kubernetes.io/pod-name"
	podUIDExtra          = "authentication.kubernetes.io/pod-uid"

	// maxCachedTokens bounds the TokenReview cache.
	maxCachedTokens = 10000
	// maxTokenCacheTime bounds how long one TokenReview result is reused,
	// whatever the token's expiry.
	maxTokenCacheTime = time.Hour
)

// ServiceAccountOptions enables sign-in with Kubernetes projected
// service-account tokens.
type ServiceAccountOptions struct {
	// Client runs TokenReviews and reads pods and replicasets.
	Client kubernetes.Interface
	// Namespaces lists the namespaces whose service accounts may sign in.
	Namespaces []string
	// Now returns the current time. Nil uses time.Now.
	Now func() time.Time
}

// serviceAccountVerifier turns a projected service-account token into an
// attested identity.
type serviceAccountVerifier struct {
	client     kubernetes.Interface
	namespaces []string
	now        func() time.Time

	mutex sync.Mutex
	cache map[[sha256.Size]byte]cachedIdentity
}

type cachedIdentity struct {
	identity Identity
	expires  time.Time
}

func newServiceAccountVerifier(options ServiceAccountOptions) (*serviceAccountVerifier, error) {
	if options.Client == nil {
		return nil, errors.New("auth: the Kubernetes client is required for service-account sign-in")
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &serviceAccountVerifier{
		client:     options.Client,
		namespaces: slices.Clone(options.Namespaces),
		now:        now,
		cache:      map[[sha256.Size]byte]cachedIdentity{},
	}, nil
}

// verify checks the token with TokenReview and builds its identity. The
// identity has no session; the workspace is the pod's owner.
func (v *serviceAccountVerifier) verify(ctx context.Context, token string) (Identity, *Refusal) {
	key := sha256.Sum256([]byte(token))
	now := v.now()

	if identity, ok := v.cached(key, now); ok {
		return identity, nil
	}

	review, err := v.client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{ServiceAccountAudience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return Identity{}, &Refusal{Reason: "token review failed"}
	}

	if !review.Status.Authenticated || !slices.Contains(review.Status.Audiences, ServiceAccountAudience) {
		refusal := unverifiedRefusal(token)
		refusal.Reason = "invalid service-account token"

		return Identity{}, &refusal
	}

	namespace, _, ok := splitServiceAccount(review.Status.User.Username)
	if !ok {
		return Identity{}, &Refusal{Reason: "not a service account"}
	}

	if !slices.Contains(v.namespaces, namespace) {
		return Identity{}, &Refusal{Reason: "namespace not allowed"}
	}

	podName := singleExtra(review.Status.User.Extra, podNameExtra)
	podUID := singleExtra(review.Status.User.Extra, podUIDExtra)

	if podName == "" || podUID == "" {
		return Identity{}, &Refusal{Reason: "token not bound to a pod"}
	}

	workspace, err := v.podWorkspace(ctx, namespace, podName, podUID)
	if err != nil {
		return Identity{}, &Refusal{Reason: err.Error()}
	}

	identity := Identity{
		Principal:   review.Status.User.Username,
		Workspace:   workspace,
		Attestation: AttestationAttested,
	}

	v.store(key, identity, tokenExpiry(token, now))

	return identity, nil
}

// podWorkspace returns `k8s/<namespace>/<owner kind>/<owner name>` for the
// pod. The pod's UID must match the token's, so a token from a deleted pod
// can't speak for a new pod with the same name. A ReplicaSet owned by a
// Deployment reports the Deployment. A pod without a controller reports
// itself.
func (v *serviceAccountVerifier) podWorkspace(ctx context.Context, namespace, podName, podUID string) (string, error) {
	pod, err := v.client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", errors.New("pod lookup failed")
	}

	if string(pod.UID) != podUID {
		return "", errors.New("pod UID mismatch")
	}

	kind, name := "Pod", pod.Name

	if owner := metav1.GetControllerOf(pod); owner != nil {
		kind, name = owner.Kind, owner.Name

		if owner.Kind == "ReplicaSet" && strings.HasPrefix(owner.APIVersion, "apps/") {
			replicaSet, err := v.client.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				return "", errors.New("replicaset lookup failed")
			}

			if replicaSet.UID != owner.UID {
				return "", errors.New("replicaset UID mismatch")
			}

			if deployment := metav1.GetControllerOf(replicaSet); deployment != nil && deployment.Kind == "Deployment" {
				kind, name = deployment.Kind, deployment.Name
			}
		}
	}

	return fmt.Sprintf("k8s/%s/%s/%s", namespace, kind, name), nil
}

func (v *serviceAccountVerifier) cached(key [sha256.Size]byte, now time.Time) (Identity, bool) {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	entry, ok := v.cache[key]
	if !ok {
		return Identity{}, false
	}

	if !now.Before(entry.expires) {
		delete(v.cache, key)

		return Identity{}, false
	}

	return entry.identity, true
}

func (v *serviceAccountVerifier) store(key [sha256.Size]byte, identity Identity, expires time.Time) {
	now := v.now()
	if !now.Before(expires) {
		return
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	if len(v.cache) >= maxCachedTokens {
		for existing, entry := range v.cache {
			if !now.Before(entry.expires) {
				delete(v.cache, existing)
			}
		}
	}

	if len(v.cache) >= maxCachedTokens {
		return
	}

	v.cache[key] = cachedIdentity{identity: identity, expires: expires}
}

// tokenExpiry reads the expiry of a token TokenReview has accepted, capped at
// maxTokenCacheTime from now. A token without an expiry isn't cached.
func tokenExpiry(token string, now time.Time) time.Time {
	parsed, err := jwt.ParseSigned(token, tokenAlgorithms)
	if err != nil {
		return now
	}

	var claims jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil || claims.Expiry == nil {
		return now
	}

	limit := now.Add(maxTokenCacheTime)
	if expiry := claims.Expiry.Time(); expiry.Before(limit) {
		return expiry
	}

	return limit
}

// unverifiedExpiry reads the expiry claim of a token that was already
// verified, or zero when it has none.
func unverifiedExpiry(token string) time.Time {
	parsed, err := jwt.ParseSigned(token, tokenAlgorithms)
	if err != nil {
		return time.Time{}
	}

	var claims jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil || claims.Expiry == nil {
		return time.Time{}
	}

	return claims.Expiry.Time()
}

// splitServiceAccount splits `system:serviceaccount:<ns>:<name>`.
func splitServiceAccount(username string) (namespace, name string, ok bool) {
	rest, found := strings.CutPrefix(username, serviceAccountPrefix)
	if !found {
		return "", "", false
	}

	namespace, name, found = strings.Cut(rest, ":")
	if !found || namespace == "" || name == "" || strings.Contains(name, ":") {
		return "", "", false
	}

	return namespace, name, true
}

func singleExtra(extra map[string]authenticationv1.ExtraValue, key string) string {
	values := extra[key]
	if len(values) != 1 {
		return ""
	}

	return values[0]
}
