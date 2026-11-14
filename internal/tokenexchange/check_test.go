package tokenexchange_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/tokenexchange"
)

const (
	secretsNamespace = "toolgate-template-secrets"
	allowedTokenURL  = "https://idp.example/application/o/token/"
	// secretValue is the client secret; no message may hold it.
	secretValue = "s3cr3t-client-value"
)

func scheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()

	result := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(result))
	require.NoError(t, v1alpha1.AddToScheme(result))

	return result
}

func clientSecrets(t *testing.T) tokenexchange.SecretSource {
	t.Helper()

	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: secretsNamespace, Name: "exchange-client"},
			Data: map[string][]byte{
				"client-secret": []byte(secretValue + "\n"),
				"empty":         {},
				"blank":         []byte(" \n"),
			},
		},
	).Build()

	return runtime.APITemplateSecrets{Reader: reader, Namespace: secretsNamespace}
}

func validExchange() *v1alpha1.TokenExchange {
	return &v1alpha1.TokenExchange{
		ObjectMeta: metav1.ObjectMeta{Namespace: "catalog", Name: "temporal"},
		Spec: v1alpha1.TokenExchangeSpec{
			TokenURL:        allowedTokenURL,
			ClientID:        "toolgate-token-exchange",
			ClientSecretRef: v1alpha1.SecretKeyRef{Name: "exchange-client", Key: "client-secret"},
		},
	}
}

func TestCheckReportsWhyAnExchangeIsInvalid(t *testing.T) {
	tests := []struct {
		name         string
		change       func(exchange *v1alpha1.TokenExchange)
		pastLimit    bool
		noKeyset     bool
		noSecrets    bool
		wantReason   string
		wantMessages []string
	}{
		{name: "valid", wantReason: tokenexchange.ReasonValid},
		{name: "valid with every field", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.Audience, exchange.Spec.Scope, exchange.Spec.RequestedTokenType = "temporal", "openid temporal", "urn:ietf:params:oauth:token-type:access_token"
			exchange.Spec.Timeout = &metav1.Duration{Duration: 60 * time.Second}
		}, wantReason: tokenexchange.ReasonValid},
		{name: "http tokenURL", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.TokenURL = "http://idp.example/application/o/token/"
		}, wantReason: tokenexchange.ReasonBadFields, wantMessages: []string{"tokenURL", "https://"}},
		{name: "no clientID", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.ClientID = ""
		}, wantReason: tokenexchange.ReasonBadFields, wantMessages: []string{"clientID: required"}},
		{name: "no Secret name", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.ClientSecretRef.Name = ""
		}, wantReason: tokenexchange.ReasonBadFields, wantMessages: []string{"clientSecretRef.name: required"}},
		{name: "no Secret key", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.ClientSecretRef.Key = ""
		}, wantReason: tokenexchange.ReasonBadFields, wantMessages: []string{"clientSecretRef.key: required"}},
		{name: "timeout over 60s", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.Timeout = &metav1.Duration{Duration: 60*time.Second + time.Millisecond}
		}, wantReason: tokenexchange.ReasonBadFields, wantMessages: []string{"timeout"}},
		{name: "timeout of 0", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.Timeout = &metav1.Duration{}
		}, wantReason: tokenexchange.ReasonBadFields, wantMessages: []string{"timeout"}},
		{name: "past the namespace limit", pastLimit: true, wantReason: tokenexchange.ReasonTooMany, wantMessages: []string{"catalog", "16"}},
		{name: "no keyset", noKeyset: true, wantReason: tokenexchange.ReasonNoKeyset, wantMessages: []string{"secretsKeysetFile"}},
		{name: "tokenURL not allowed", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.TokenURL = "https://collector.example/token"
		}, wantReason: tokenexchange.ReasonURLNotAllowed, wantMessages: []string{"https://collector.example/token", "allow-list"}},
		{name: "tokenURL with the allowed URL as prefix", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.TokenURL = allowedTokenURL + "x"
		}, wantReason: tokenexchange.ReasonURLNotAllowed},
		{name: "Secret missing", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.ClientSecretRef.Name = "absent"
		}, wantReason: tokenexchange.ReasonSecretMissing, wantMessages: []string{"`absent`", secretsNamespace}},
		{name: "key missing", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.ClientSecretRef.Key = "other"
		}, wantReason: tokenexchange.ReasonSecretMissing, wantMessages: []string{"`exchange-client`", "`other`"}},
		{name: "key empty", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.ClientSecretRef.Key = "empty"
		}, wantReason: tokenexchange.ReasonSecretMissing, wantMessages: []string{"`exchange-client`", "`empty`", "is empty"}},
		{name: "key only white space", change: func(exchange *v1alpha1.TokenExchange) {
			exchange.Spec.ClientSecretRef.Key = "blank"
		}, wantReason: tokenexchange.ReasonSecretMissing, wantMessages: []string{"`exchange-client`", "`blank`", "is empty"}},
		{name: "no template secrets namespace", noSecrets: true, wantReason: tokenexchange.ReasonSecretMissing, wantMessages: []string{"templateSecretsNamespace", "`exchange-client`", "`client-secret`"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checker := tokenexchange.Checker{
				AllowList: tokenexchange.StaticAllowList{allowedTokenURL},
				Secrets:   clientSecrets(t),
				HasKeyset: !test.noKeyset,
			}
			if test.noSecrets {
				checker.Secrets = nil
			}

			exchange := validExchange()
			if test.change != nil {
				test.change(exchange)
			}

			result, err := checker.Check(t.Context(), exchange, test.pastLimit)
			require.NoError(t, err)

			assert.Equal(t, test.wantReason, result.Reason)
			assert.Equal(t, test.wantReason == tokenexchange.ReasonValid, result.Valid())
			assert.NotContains(t, result.Message, secretValue, "the message never holds the client secret")

			for _, want := range test.wantMessages {
				assert.Contains(t, result.Message, want)
			}
		})
	}
}

type failingAllowList struct{}

func (failingAllowList) Allowed(context.Context, string) (bool, error) {
	return false, errors.New("discovery failed")
}

func TestCheckReturnsAnErrorWhenTheAllowListCannotBeRead(t *testing.T) {
	checker := tokenexchange.Checker{AllowList: failingAllowList{}, Secrets: clientSecrets(t), HasKeyset: true}

	_, err := checker.Check(t.Context(), validExchange(), false)
	require.ErrorContains(t, err, "discovery failed")
}
