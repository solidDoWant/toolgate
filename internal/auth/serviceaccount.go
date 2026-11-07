package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ServiceAccountAudience is the audience a projected service-account token
// must carry for the gateway.
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

	cache *tokenCache[Identity]
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
		cache:      newTokenCache[Identity](),
	}, nil
}

// verify checks the token with TokenReview and builds its identity. The
// identity has no session; the workspace is the pod's owner.
func (v *serviceAccountVerifier) verify(ctx context.Context, token string) (Identity, *Refusal) {
	key := sha256.Sum256([]byte(token))
	now := v.now()

	if identity, ok := v.cache.get(key, now); ok {
		return identity, nil
	}

	reviewed, err := v.reviewPod(ctx, token)
	if err != nil {
		var failure *podReviewError
		if !errors.As(err, &failure) {
			return Identity{}, &Refusal{Reason: "token review failed"}
		}

		if failure.stage == stageTokenInvalid {
			refusal := unverifiedRefusal(token)
			refusal.Reason = failure.reason

			return Identity{}, &refusal
		}

		return Identity{}, &Refusal{Reason: failure.reason}
	}

	workspace, err := v.podWorkspace(ctx, reviewed.pod)
	if err != nil {
		return Identity{}, &Refusal{Reason: err.Error()}
	}

	identity := Identity{
		Principal:   reviewed.username,
		Workspace:   workspace,
		Kind:        KindServiceAccount,
		Attestation: AttestationAttested,
	}

	v.cache.put(key, identity, now, tokenExpiry(token, now))

	return identity, nil
}

// Stages of a pod review, which each caller names in its own words.
const (
	stageReviewFailed = iota
	stageTokenInvalid
	stageNotServiceAccount
	stageNamespace
	stageNotBound
	stagePodLookup
	stagePodUID
)

// podReviewError is a pod review that refused the token, or couldn't
// decide when unavailable is set: a transport error or a 5xx from the API
// server.
type podReviewError struct {
	stage       int
	reason      string
	unavailable bool
}

func (e *podReviewError) Error() string { return e.reason }

// reviewedPod is a token TokenReview accepted, from an allowed namespace,
// bound to the pod it names.
type reviewedPod struct {
	username string
	pod      *corev1.Pod
}

// reviewPod checks the token with TokenReview and audience toolgate, checks
// its namespace and its pod binding, and gets the pod. The pod's UID must
// match the token's, so a token from a deleted pod can't speak for a new pod
// with the same name.
func (v *serviceAccountVerifier) reviewPod(ctx context.Context, token string) (reviewedPod, error) {
	review, err := v.client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{ServiceAccountAudience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return reviewedPod{}, &podReviewError{stage: stageReviewFailed, reason: "token review failed", unavailable: unavailable(err)}
	}

	if !review.Status.Authenticated || !slices.Contains(review.Status.Audiences, ServiceAccountAudience) {
		return reviewedPod{}, &podReviewError{stage: stageTokenInvalid, reason: "invalid service-account token"}
	}

	namespace, _, ok := splitServiceAccount(review.Status.User.Username)
	if !ok {
		return reviewedPod{}, &podReviewError{stage: stageNotServiceAccount, reason: "not a service account"}
	}

	if !slices.Contains(v.namespaces, namespace) {
		return reviewedPod{}, &podReviewError{stage: stageNamespace, reason: "namespace not allowed"}
	}

	podName := singleExtra(review.Status.User.Extra, podNameExtra)
	podUID := singleExtra(review.Status.User.Extra, podUIDExtra)

	if podName == "" || podUID == "" {
		return reviewedPod{}, &podReviewError{stage: stageNotBound, reason: "token not bound to a pod"}
	}

	pod, err := v.client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return reviewedPod{}, &podReviewError{stage: stagePodLookup, reason: "pod lookup failed", unavailable: unavailable(err)}
	}

	if string(pod.UID) != podUID {
		return reviewedPod{}, &podReviewError{stage: stagePodUID, reason: "pod UID mismatch"}
	}

	return reviewedPod{username: review.Status.User.Username, pod: pod}, nil
}

// unavailable reports whether an API call failed without an answer: a
// transport error, or a 5xx status.
func unavailable(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return true
	}

	return status.Status().Code >= http.StatusInternalServerError
}

// podWorkspace returns `k8s/<namespace>/<owner kind>/<owner name>` for the
// pod. A ReplicaSet owned by a
// Deployment reports the Deployment. A pod without a controller reports
// itself.
func (v *serviceAccountVerifier) podWorkspace(ctx context.Context, pod *corev1.Pod) (string, error) {
	namespace := pod.Namespace
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

// tokenCache holds what was learned from a token, keyed by its SHA-256,
// until the token's expiry, at most maxTokenCacheTime.
type tokenCache[T any] struct {
	mutex   sync.Mutex
	entries map[[sha256.Size]byte]tokenCacheEntry[T]
}

type tokenCacheEntry[T any] struct {
	value   T
	expires time.Time
}

func newTokenCache[T any]() *tokenCache[T] {
	return &tokenCache[T]{entries: map[[sha256.Size]byte]tokenCacheEntry[T]{}}
}

func (c *tokenCache[T]) get(key [sha256.Size]byte, now time.Time) (T, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		var zero T

		return zero, false
	}

	if !now.Before(entry.expires) {
		delete(c.entries, key)

		var zero T

		return zero, false
	}

	return entry.value, true
}

func (c *tokenCache[T]) put(key [sha256.Size]byte, value T, now, expires time.Time) {
	if !now.Before(expires) {
		return
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	if len(c.entries) >= maxCachedTokens {
		for existing, entry := range c.entries {
			if !now.Before(entry.expires) {
				delete(c.entries, existing)
			}
		}
	}

	if len(c.entries) >= maxCachedTokens {
		return
	}

	c.entries[key] = tokenCacheEntry[T]{value: value, expires: expires}
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
