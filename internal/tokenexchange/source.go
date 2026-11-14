package tokenexchange

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/credentials"
)

// Source builds the exchanger of a TokenExchange resource each time an
// exchange runs, so an edit to the resource applies from the next exchange.
type Source struct {
	// Reader reads TokenExchange resources, usually from the cache.
	Reader client.Reader
	// AllowList holds the token endpoints exchanges may use. Source checks
	// it again before every exchange, so a status written before an edit
	// never lets a token go to a URL that isn't allowed.
	AllowList AllowList
	// Secrets reads client secrets from the template secrets namespace.
	// Nil means the gateway has none, and every exchange fails.
	Secrets SecretSource
	// Transport sends the exchange requests. It holds the trust roots the
	// chart sets; a resource never chooses them.
	Transport http.RoundTripper
	// Metrics and Logger may be nil.
	Metrics *credentials.Metrics
	Logger  *slog.Logger
}

// Exchanger returns the exchanger of the TokenExchange name in namespace,
// built from the resource as it is now. It fails when the resource is
// missing, isn't Valid for its current generation, has bad fields, or names
// a tokenURL that isn't on the allow-list; nothing is then sent anywhere.
func (s *Source) Exchanger(ctx context.Context, namespace, name string) (*credentials.Exchanger, error) {
	exchange, err := s.resource(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	if problems := fieldProblems(&exchange.Spec); len(problems) > 0 {
		return nil, fmt.Errorf("token exchange `%s` has bad fields: %s", name, strings.Join(problems, "; "))
	}

	if s.AllowList == nil {
		return nil, fmt.Errorf("token exchange `%s`: the gateway has no allow-list of token endpoints", name)
	}

	allowed, err := s.AllowList.Allowed(ctx, exchange.Spec.TokenURL)
	if err != nil {
		return nil, fmt.Errorf("token exchange `%s`: reading the allow-list: %w", name, err)
	}

	if !allowed {
		return nil, fmt.Errorf("token exchange `%s`: tokenURL %s is not on the gateway's allow-list of token endpoints", name, exchange.Spec.TokenURL)
	}

	timeout := DefaultTimeout
	if exchange.Spec.Timeout != nil {
		timeout = exchange.Spec.Timeout.Duration
	}

	ref := exchange.Spec.ClientSecretRef
	spec := credentials.ExchangeSpec{
		Name:               name,
		TokenURL:           exchange.Spec.TokenURL,
		ClientID:           exchange.Spec.ClientID,
		Audience:           exchange.Spec.Audience,
		Scope:              exchange.Spec.Scope,
		RequestedTokenType: exchange.Spec.RequestedTokenType,
		Timeout:            timeout,
		ClientSecret: func(ctx context.Context) ([]byte, error) {
			return s.clientSecret(ctx, ref)
		},
	}

	return credentials.NewExchanger(spec, s.Transport, s.Metrics, s.Logger), nil
}

// resource reads the TokenExchange and checks its Valid condition.
func (s *Source) resource(ctx context.Context, namespace, name string) (*v1alpha1.TokenExchange, error) {
	exchange := &v1alpha1.TokenExchange{}

	err := s.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, exchange)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("token exchange `%s` doesn't exist in namespace %s", name, namespace)
	}

	if err != nil {
		return nil, fmt.Errorf("token exchange `%s`: reading the resource: %w", name, err)
	}

	if problem := ValidityProblem(exchange); problem != "" {
		return nil, errors.New(problem)
	}

	return exchange, nil
}

// clientSecret reads the client secret. Its errors name the Secret and the
// key, never the value.
func (s *Source) clientSecret(ctx context.Context, ref v1alpha1.SecretKeyRef) ([]byte, error) {
	if s.Secrets == nil {
		return nil, fmt.Errorf("the gateway has no templateSecretsNamespace, so it can't read key `%s` of Secret `%s`", ref.Key, ref.Name)
	}

	value, _, err := s.Secrets.TemplateSecret(ctx, ref.Name, ref.Key)
	if err != nil {
		return nil, err
	}

	if strings.TrimRight(string(value), " \t\r\n") == "" {
		return nil, fmt.Errorf("key `%s` of Secret `%s` is empty", ref.Key, ref.Name)
	}

	return value, nil
}

// ValidityProblem returns why exchange can't be used now, or "" when its
// Valid condition is True for its current generation. A condition written
// for an older generation hasn't checked the current spec yet.
func ValidityProblem(exchange *v1alpha1.TokenExchange) string {
	condition := meta.FindStatusCondition(exchange.Status.Conditions, ConditionValid)

	switch {
	case condition == nil || condition.ObservedGeneration != exchange.Generation:
		return fmt.Sprintf("token exchange `%s` hasn't been checked yet", exchange.Name)
	case condition.Status != "True":
		return fmt.Sprintf("token exchange `%s` isn't valid: %s", exchange.Name, condition.Message)
	}

	return ""
}

// Lookup tells the catalog validator whether a template may use a token
// exchange.
type Lookup struct {
	// Reader reads TokenExchange resources from the cache.
	Reader client.Reader
}

// ExchangeProblem returns why a template in namespace can't use the token
// exchange name, or "" when it can. Only a TokenExchange in the template's
// own namespace counts. It reads the cache, which doesn't block, so it
// takes no context.
func (l Lookup) ExchangeProblem(namespace, name string) string {
	exchange := &v1alpha1.TokenExchange{}

	err := l.Reader.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, exchange)
	if apierrors.IsNotFound(err) {
		return fmt.Sprintf("token exchange `%s` doesn't exist in namespace %s", name, namespace)
	}

	if err != nil {
		return fmt.Sprintf("token exchange `%s` can't be read: %s", name, err)
	}

	return ValidityProblem(exchange)
}
