package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// Attested user sessions: a user token in
// Authorization plus a workload token in AttestationHeader that is bound to
// that user.
const (
	// AttestationHeader carries the workload token, raw, without Bearer.
	AttestationHeader = "X-Toolgate-Attestation"
	// MaxAttestationBytes bounds the attestation header, as Authorization.
	MaxAttestationBytes = 16 << 10

	// MaxAttestedWorkspaceLength bounds a rendered workspaceID.
	MaxAttestedWorkspaceLength = 256

	// Match modes.
	MatchByEmail = "email"
	MatchByClaim = "claim"
)

// Attestation key names: the placeholders of workspaceID and
// spiffeIDPattern, and the facts read from a pod.
const (
	AttestationKeyUserID    = "userID"
	AttestationKeyEmail     = "email"
	AttestationKeyUsername  = "username"
	AttestationKeyWorkspace = "workspace"
)

// Default pod label keys: those of Coder's templates.
const (
	DefaultAttestationUserIDKey    = "com.coder.user.id"
	DefaultAttestationEmailKey     = "com.coder.user.email"
	DefaultAttestationUsernameKey  = "com.coder.user.username"
	DefaultAttestationWorkspaceKey = "com.coder.workspace.name"
)

// The refusal reasons of a failed attestation.
const (
	reasonAttestationInvalid      = "attestation token invalid"
	reasonAttestationNamespace    = "attestation namespace not allowed"
	reasonAttestationNotBound     = "attestation not bound to a pod"
	reasonAttestationPodLookup    = "attestation pod lookup failed"
	reasonAttestationKeyMissing   = "attestation owner key missing"
	reasonAttestationMismatch     = "attestation owner mismatch"
	reasonAttestationClaimMissing = "attestation owner claim missing"
	reasonAttestationWorkspaceKey = "attestation workspace key invalid"
	reasonAttestationSPIFFEID     = "attestation SPIFFE ID doesn't match the pattern"
	reasonAttestationHeader       = "attestation header repeated or too large"
)

// attestationValuePattern is the syntax of every value that fills a
// placeholder: a label value's, so a value can't add path segments.
var attestationValuePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,63}$`)

// placeholderPattern finds `{name}` in workspaceID and spiffeIDPattern.
var placeholderPattern = regexp.MustCompile(`\{([^{}]*)\}`)

// workspacePlaceholders are the placeholders workspaceID and
// spiffeIDPattern may use.
var workspacePlaceholders = []string{AttestationKeyUserID, AttestationKeyUsername, AttestationKeyWorkspace}

// AttestationOptions enables attested user sessions.
type AttestationOptions struct {
	// Client runs TokenReviews and reads pods.
	Client kubernetes.Interface
	// Namespaces lists the namespaces whose service-account tokens may
	// attest a user.
	Namespaces []string
	// Keys are the pod label (or annotation) keys of each fact. Empty keys
	// take Coder's.
	Keys AttestationKeys
	// MatchBy is MatchByEmail or MatchByClaim.
	MatchBy string
	// MatchClaim is the user token's claim compared with the MatchKey fact
	// when MatchBy is MatchByClaim.
	MatchClaim string
	// MatchKey is AttestationKeyUserID or AttestationKeyUsername.
	MatchKey string
	// WorkspaceID is the verified workspace, with placeholders.
	WorkspaceID string
	// SPIFFEIDPattern, if set, accepts JWT-SVIDs whose ID matches it.
	SPIFFEIDPattern string
	// SPIFFETrustDomains are the configured trust domains; the pattern's
	// must be one of them.
	SPIFFETrustDomains []string
	// Now returns the current time. Nil uses time.Now.
	Now func() time.Time
}

// AttestationKeys are the pod label keys of the facts.
type AttestationKeys struct {
	UserID    string
	Email     string
	Username  string
	Workspace string
}

// attestationRules is the compiled configuration.
type attestationRules struct {
	keys         map[string]string
	matchBy      string
	matchClaim   string
	matchKey     string
	workspaceID  string
	placeholders []string
	spiffeID     *regexp.Regexp
}

// ValidateAttestation returns every problem with options. Client and Now
// aren't checked.
func ValidateAttestation(options AttestationOptions) []error {
	_, problems := compileAttestation(options)

	return problems
}

func compileAttestation(options AttestationOptions) (*attestationRules, []error) {
	var problems []error

	if len(options.Namespaces) == 0 {
		problems = append(problems, errors.New("namespaces: required"))
	}

	for _, namespace := range options.Namespaces {
		if invalid := validation.IsDNS1123Label(namespace); len(invalid) > 0 {
			problems = append(problems, fmt.Errorf("namespaces: %q is not a namespace name", namespace))
		}
	}

	rules := &attestationRules{
		keys: map[string]string{
			AttestationKeyUserID:    valueOr(options.Keys.UserID, DefaultAttestationUserIDKey),
			AttestationKeyEmail:     valueOr(options.Keys.Email, DefaultAttestationEmailKey),
			AttestationKeyUsername:  valueOr(options.Keys.Username, DefaultAttestationUsernameKey),
			AttestationKeyWorkspace: valueOr(options.Keys.Workspace, DefaultAttestationWorkspaceKey),
		},
		matchBy:     options.MatchBy,
		matchClaim:  options.MatchClaim,
		matchKey:    options.MatchKey,
		workspaceID: options.WorkspaceID,
	}

	for _, name := range []string{AttestationKeyUserID, AttestationKeyEmail, AttestationKeyUsername, AttestationKeyWorkspace} {
		if invalid := validation.IsQualifiedName(rules.keys[name]); len(invalid) > 0 {
			problems = append(problems, fmt.Errorf("keys.%s: %q is not a label key", name, rules.keys[name]))
		}
	}

	switch options.MatchBy {
	case MatchByEmail:
	case MatchByClaim:
		if options.MatchClaim == "" {
			problems = append(problems, errors.New("match.claim: required with match.by claim"))
		}

		if options.MatchKey != AttestationKeyUserID && options.MatchKey != AttestationKeyUsername {
			problems = append(problems, fmt.Errorf("match.key: %q must be userID or username", options.MatchKey))
		}
	default:
		problems = append(problems, fmt.Errorf("match.by: %q must be email or claim", options.MatchBy))
	}

	placeholders, err := parseWorkspaceID(options.WorkspaceID)
	if err != nil {
		problems = append(problems, fmt.Errorf("workspaceID: %w", err))
	}

	rules.placeholders = placeholders

	if options.SPIFFEIDPattern != "" {
		pattern, used, err := compileSPIFFEIDPattern(options.SPIFFEIDPattern, options.SPIFFETrustDomains)
		if err != nil {
			problems = append(problems, fmt.Errorf("spiffeIDPattern: %w", err))
		} else {
			rules.spiffeID = pattern

			if options.MatchBy != MatchByClaim {
				problems = append(problems, errors.New("match.by: must be claim with spiffeIDPattern, because SPIFFE IDs can't hold an email"))
			} else if !slices.Contains(used, options.MatchKey) {
				problems = append(problems, fmt.Errorf("spiffeIDPattern: must contain {%s}, the match key", options.MatchKey))
			}

			for _, placeholder := range placeholders {
				if !slices.Contains(used, placeholder) {
					problems = append(problems, fmt.Errorf("spiffeIDPattern: must contain {%s}, which workspaceID uses", placeholder))
				}
			}
		}
	}

	return rules, problems
}

// parseWorkspaceID checks workspaceID and returns the placeholders it uses.
// Its first segment must be literal and not `claimed`, `k8s` or empty, so a
// verified workspace never looks like a claimed one or a pod's.
func parseWorkspaceID(workspaceID string) ([]string, error) {
	if workspaceID == "" {
		return nil, errors.New("required")
	}

	var placeholders []string

	last := 0

	for _, match := range placeholderPattern.FindAllStringSubmatchIndex(workspaceID, -1) {
		if strings.ContainsAny(workspaceID[last:match[0]], "{}") {
			return nil, fmt.Errorf("%q has an unmatched brace", workspaceID)
		}

		name := workspaceID[match[2]:match[3]]
		if !slices.Contains(workspacePlaceholders, name) {
			return nil, fmt.Errorf("%q: {%s} is not one of {userID}, {username} and {workspace}", workspaceID, name)
		}

		if !slices.Contains(placeholders, name) {
			placeholders = append(placeholders, name)
		}

		last = match[1]
	}

	if strings.ContainsAny(workspaceID[last:], "{}") {
		return nil, fmt.Errorf("%q has an unmatched brace", workspaceID)
	}

	first, _, _ := strings.Cut(workspaceID, "/")
	if strings.ContainsAny(first, "{}") || first == "" || first == "claimed" || first == "k8s" {
		return nil, fmt.Errorf("%q must start with a literal segment that isn't claimed, k8s or empty", workspaceID)
	}

	if len(workspaceID) > MaxAttestedWorkspaceLength {
		return nil, fmt.Errorf("%q is longer than %d characters", workspaceID, MaxAttestedWorkspaceLength)
	}

	return placeholders, nil
}

// compileSPIFFEIDPattern turns the pattern into a regexp: the literal parts
// quoted and one named group per placeholder, each a whole path segment.
func compileSPIFFEIDPattern(pattern string, trustDomains []string) (*regexp.Regexp, []string, error) {
	rest, found := strings.CutPrefix(pattern, spiffeScheme)
	if !found {
		return nil, nil, fmt.Errorf("%q must start with spiffe://", pattern)
	}

	segments := strings.Split(rest, "/")
	if len(segments) < 2 {
		return nil, nil, fmt.Errorf("%q has no path", pattern)
	}

	trustDomain, err := spiffeid.TrustDomainFromString(segments[0])
	if err != nil {
		return nil, nil, fmt.Errorf("%q: %w", pattern, err)
	}

	if !slices.Contains(trustDomains, trustDomain.Name()) {
		return nil, nil, fmt.Errorf("%q: trust domain %s is not in spiffeTrustDomains", pattern, trustDomain.Name())
	}

	expression := "^" + regexp.QuoteMeta(spiffeScheme+trustDomain.Name())

	var used []string

	for _, segment := range segments[1:] {
		name, isPlaceholder := strings.CutPrefix(segment, "{")
		name, closed := strings.CutSuffix(name, "}")

		switch {
		case isPlaceholder && closed && !strings.ContainsAny(name, "{}"):
			if !slices.Contains(workspacePlaceholders, name) {
				return nil, nil, fmt.Errorf("%q: {%s} is not one of {userID}, {username} and {workspace}", pattern, name)
			}

			if slices.Contains(used, name) {
				return nil, nil, fmt.Errorf("%q uses {%s} twice", pattern, name)
			}

			used = append(used, name)
			expression += "/(?P<" + name + ">[A-Za-z0-9._-]+)"
		case segment == "" || strings.ContainsAny(segment, "{}"):
			return nil, nil, fmt.Errorf("%q: each placeholder must be one whole path segment, and no segment may be empty", pattern)
		default:
			expression += "/" + regexp.QuoteMeta(segment)
		}
	}

	compiled, err := regexp.Compile(expression + "$")
	if err != nil {
		return nil, nil, fmt.Errorf("%q: %w", pattern, err)
	}

	return compiled, used, nil
}

// attestationVerifier verifies and binds attestation tokens.
type attestationVerifier struct {
	rules    *attestationRules
	accounts *serviceAccountVerifier
	spiffe   *spiffeVerifier
	cache    *tokenCache[map[string]string]
}

func newAttestationVerifier(options AttestationOptions, spiffe *spiffeVerifier) (*attestationVerifier, error) {
	rules, problems := compileAttestation(options)
	if len(problems) > 0 {
		return nil, fmt.Errorf("auth: workspaceAttestation: %w", errors.Join(problems...))
	}

	accounts, err := newServiceAccountVerifier(ServiceAccountOptions{
		Client: options.Client, Namespaces: options.Namespaces, Now: options.Now,
	})
	if err != nil {
		return nil, err
	}

	return &attestationVerifier{rules: rules, accounts: accounts, spiffe: spiffe, cache: newTokenCache[map[string]string]()}, nil
}

// errAttestationUnavailable means the check couldn't run: TokenReview or the
// pod GET failed with a transport error or a 5xx.
var errAttestationUnavailable = errors.New("attestation check unavailable")

// attest verifies the request's attestation header, if any, and binds it to
// the user. It returns the attested identity; the user's identity with a
// refusal for a failed verdict; or errAttestationUnavailable.
func (v *attestationVerifier) attest(ctx context.Context, request *http.Request, identity Identity,
	claims map[string]json.RawMessage,
) (Identity, *Refusal, error) {
	values := request.Header.Values(AttestationHeader)

	switch {
	case len(values) == 0 || (len(values) == 1 && values[0] == ""):
		return identity, nil, nil
	case len(values) > 1 || len(values[0]) > MaxAttestationBytes:
		return identity, &Refusal{Reason: reasonAttestationHeader}, nil
	}

	token := values[0]

	facts, reason, err := v.facts(ctx, token)
	if err != nil {
		return identity, nil, err
	}

	if reason == "" {
		var workspace string

		workspace, reason = v.bind(facts, identity, claims)
		if reason == "" {
			identity.Attestation = AttestationAttested
			identity.Workspace = workspace

			return identity, nil, nil
		}
	}

	return identity, &Refusal{Reason: reason}, nil
}

// facts verifies the token and returns its owner and workspace facts, or
// the refusal reason.
func (v *attestationVerifier) facts(ctx context.Context, token string) (map[string]string, string, error) {
	_, subject := unverifiedIssuerAndSubject(token)
	if isSPIFFESubject(subject) {
		facts, reason := v.spiffeFacts(ctx, token, subject)

		return facts, reason, nil
	}

	key := sha256.Sum256([]byte(token))
	now := v.accounts.now()

	if facts, ok := v.cache.get(key, now); ok {
		return facts, "", nil
	}

	reviewed, err := v.accounts.reviewPod(ctx, token)
	if err != nil {
		var failure *podReviewError
		if !errors.As(err, &failure) {
			return nil, reasonAttestationInvalid, nil
		}

		if failure.unavailable {
			return nil, "", errAttestationUnavailable
		}

		switch failure.stage {
		case stageNamespace:
			return nil, reasonAttestationNamespace, nil
		case stageNotBound:
			return nil, reasonAttestationNotBound, nil
		case stagePodLookup, stagePodUID:
			return nil, reasonAttestationPodLookup, nil
		default:
			return nil, reasonAttestationInvalid, nil
		}
	}

	facts := podFacts(reviewed.pod, v.rules.keys)
	v.cache.put(key, facts, now, tokenExpiry(token, now))

	return facts, "", nil
}

// podFacts reads each configured key from the pod's labels, or from its
// annotations when the labels lack it.
func podFacts(pod *corev1.Pod, keys map[string]string) map[string]string {
	facts := map[string]string{}

	for name, key := range keys {
		if value, ok := pod.Labels[key]; ok {
			facts[name] = value
		} else if value, ok := pod.Annotations[key]; ok {
			facts[name] = value
		}
	}

	return facts
}

// spiffeFacts verifies a JWT-SVID and returns the values its ID captures.
func (v *attestationVerifier) spiffeFacts(ctx context.Context, token, subject string) (map[string]string, string) {
	if v.rules.spiffeID == nil || v.spiffe == nil {
		return nil, reasonAttestationSPIFFEID
	}

	svid, refusal := v.spiffe.verify(ctx, token, subject)
	if refusal != nil {
		return nil, reasonAttestationInvalid
	}

	match := v.rules.spiffeID.FindStringSubmatch(svid.Principal)
	if match == nil {
		return nil, reasonAttestationSPIFFEID
	}

	facts := map[string]string{}

	for index, name := range v.rules.spiffeID.SubexpNames() {
		if name == "" {
			continue
		}

		if !attestationValuePattern.MatchString(match[index]) {
			return nil, reasonAttestationWorkspaceKey
		}

		facts[name] = match[index]
	}

	return facts, ""
}

// bind compares the facts' owner with the user token and renders the
// workspace. It runs on every request, because the user token differs per
// request.
func (v *attestationVerifier) bind(facts map[string]string, identity Identity, claims map[string]json.RawMessage) (string, string) {
	switch v.rules.matchBy {
	case MatchByEmail:
		owner, ok := facts[AttestationKeyEmail]
		if !ok {
			return "", reasonAttestationKeyMissing
		}

		// Identity.Email is set only for a verified email.
		if identity.Email == "" || !strings.EqualFold(owner, identity.Email) {
			return "", reasonAttestationMismatch
		}
	case MatchByClaim:
		var claim string
		if raw, ok := claims[v.rules.matchClaim]; !ok || json.Unmarshal(raw, &claim) != nil || claim == "" {
			return "", reasonAttestationClaimMissing
		}

		owner, ok := facts[v.rules.matchKey]
		if !ok {
			return "", reasonAttestationKeyMissing
		}

		if owner != claim {
			return "", reasonAttestationMismatch
		}
	default:
		return "", reasonAttestationMismatch
	}

	replacements := make([]string, 0, 2*len(v.rules.placeholders))

	for _, placeholder := range v.rules.placeholders {
		value, ok := facts[placeholder]
		if !ok || !attestationValuePattern.MatchString(value) {
			return "", reasonAttestationWorkspaceKey
		}

		replacements = append(replacements, "{"+placeholder+"}", value)
	}

	workspace := strings.NewReplacer(replacements...).Replace(v.rules.workspaceID)
	if len(workspace) > MaxAttestedWorkspaceLength {
		return "", reasonAttestationWorkspaceKey
	}

	return workspace, ""
}
