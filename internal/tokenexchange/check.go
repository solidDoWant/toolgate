package tokenexchange

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// The Valid condition and its reasons.
const (
	ConditionValid = "Valid"

	ReasonValid         = "Valid"
	ReasonBadFields     = "BadFields"
	ReasonTooMany       = "TooMany"
	ReasonNoKeyset      = "NoKeyset"
	ReasonURLNotAllowed = "URLNotAllowed"
	ReasonSecretMissing = "SecretMissing"
)

// Limits on TokenExchange resources.
const (
	// MaxPerNamespace is how many TokenExchange resources one catalog
	// namespace may hold. Past it, the resources after the first
	// MaxPerNamespace in name order are invalid, so the result is stable.
	MaxPerNamespace = 16
	DefaultTimeout  = 10 * time.Second
	MaxTimeout      = 60 * time.Second
)

// SecretSource reads one key of a Secret in the template secrets namespace.
// Its errors name the Secret and the key, never a value.
type SecretSource interface {
	TemplateSecret(ctx context.Context, name, key string) ([]byte, string, error)
}

// Checker decides whether one TokenExchange is valid.
type Checker struct {
	// AllowList holds the token endpoints exchanges may use.
	AllowList AllowList
	// Secrets reads client secrets. Nil means the gateway has no template
	// secrets namespace, so no client secret can be read.
	Secrets SecretSource
	// HasKeyset is whether the gateway has a secrets keyset, which it needs
	// to encrypt each exchanged token on its way to the leader.
	HasKeyset bool
}

// Result is the outcome of a check: a condition reason and a message for
// people. The message never holds a secret value.
type Result struct {
	Reason  string
	Message string
}

// Valid reports whether the resource is valid.
func (r Result) Valid() bool {
	return r.Reason == ReasonValid
}

// Check checks exchange. pastLimit says whether the resource is past the
// first MaxPerNamespace of its namespace in name order. An error means the
// check couldn't decide, for example because the allow-list couldn't be
// read; the caller tries again later.
func (c Checker) Check(ctx context.Context, exchange *v1alpha1.TokenExchange, pastLimit bool) (Result, error) {
	if problems := fieldProblems(&exchange.Spec); len(problems) > 0 {
		return Result{Reason: ReasonBadFields, Message: strings.Join(problems, "; ")}, nil
	}

	if pastLimit {
		return Result{
			Reason:  ReasonTooMany,
			Message: fmt.Sprintf("namespace %s has more than %d TokenExchange resources, and this one is not among the first %d by name", exchange.Namespace, MaxPerNamespace, MaxPerNamespace),
		}, nil
	}

	if !c.HasKeyset {
		return Result{
			Reason:  ReasonNoKeyset,
			Message: "the gateway has no secretsKeysetFile, which token exchanges need: the gateway encrypts each exchanged token on its way to the leader",
		}, nil
	}

	allowed, err := c.AllowList.Allowed(ctx, exchange.Spec.TokenURL)
	if err != nil {
		return Result{}, fmt.Errorf("token exchange %s/%s: reading the allow-list: %w", exchange.Namespace, exchange.Name, err)
	}

	if !allowed {
		return Result{
			Reason:  ReasonURLNotAllowed,
			Message: fmt.Sprintf("tokenURL %s is not on the gateway's allow-list of token endpoints (config.tokenExchange.allowedTokenURLs; by default the token endpoints of the OIDC issuers users sign in with)", exchange.Spec.TokenURL),
		}, nil
	}

	if message := c.secretProblem(ctx, exchange.Spec.ClientSecretRef); message != "" {
		return Result{Reason: ReasonSecretMissing, Message: message}, nil
	}

	return Result{Reason: ReasonValid, Message: "The token exchange is valid."}, nil
}

// secretProblem returns why the client secret can't be used, or "". The
// message names the Secret and the key, never the value.
func (c Checker) secretProblem(ctx context.Context, ref v1alpha1.SecretKeyRef) string {
	if c.Secrets == nil {
		return fmt.Sprintf("client secret: the gateway has no templateSecretsNamespace, so it can't read key `%s` of Secret `%s`", ref.Key, ref.Name)
	}

	value, _, err := c.Secrets.TemplateSecret(ctx, ref.Name, ref.Key)
	if err != nil {
		return "client secret: " + err.Error()
	}

	// The exchange trims white space at the end of the secret, so a value
	// of only white space is empty.
	if strings.TrimRight(string(value), " \t\r\n") == "" {
		return fmt.Sprintf("client secret: key `%s` of Secret `%s` is empty", ref.Key, ref.Name)
	}

	return ""
}

// fieldProblems lists what is wrong with the spec's own fields.
func fieldProblems(spec *v1alpha1.TokenExchangeSpec) []string {
	var problems []string

	if target, err := url.Parse(spec.TokenURL); err != nil || target.Scheme != "https" || target.Host == "" {
		problems = append(problems, fmt.Sprintf("tokenURL: %q must be an https:// URL", spec.TokenURL))
	}

	if spec.ClientID == "" {
		problems = append(problems, "clientID: required")
	}

	if spec.ClientSecretRef.Name == "" {
		problems = append(problems, "clientSecretRef.name: required")
	}

	if spec.ClientSecretRef.Key == "" {
		problems = append(problems, "clientSecretRef.key: required")
	}

	if spec.Timeout != nil && (spec.Timeout.Duration <= 0 || spec.Timeout.Duration > MaxTimeout) {
		problems = append(problems, fmt.Sprintf("timeout: %s must be more than 0 and at most %s", spec.Timeout.Duration, MaxTimeout))
	}

	return problems
}
