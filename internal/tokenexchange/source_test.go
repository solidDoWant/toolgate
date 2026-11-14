package tokenexchange_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/tokenexchange"
)

const subjectToken = "user-access-token"

// countingEndpoint is a TLS token endpoint that counts the requests it gets
// and answers each with a token valid for an hour.
func countingEndpoint(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var received atomic.Int32

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"exchanged","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(server.Close)

	return server, &received
}

// checkedExchange is a TokenExchange whose Valid condition is True for its
// generation, as the reconciler writes it.
func checkedExchange(namespace, name, tokenURL string) *v1alpha1.TokenExchange {
	exchange := namedExchange(namespace, name)
	exchange.Spec.TokenURL = tokenURL
	exchange.Generation = 1
	meta.SetStatusCondition(&exchange.Status.Conditions, metav1.Condition{
		Type: tokenexchange.ConditionValid, Status: metav1.ConditionTrue, Reason: tokenexchange.ReasonValid, ObservedGeneration: 1,
	})

	return exchange
}

func newSource(t *testing.T, server *httptest.Server, reader client.Reader, allowed ...string) *tokenexchange.Source {
	t.Helper()

	return &tokenexchange.Source{
		Reader:    reader,
		AllowList: tokenexchange.StaticAllowList(allowed),
		Secrets:   clientSecrets(t),
		Transport: server.Client().Transport,
	}
}

func TestSourceExchangesWithTheResourceOfTheTemplateNamespace(t *testing.T) {
	server, received := countingEndpoint(t)
	tokenURL := server.URL + "/token"

	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(checkedExchange("catalog", "temporal", tokenURL)).Build()
	source := newSource(t, server, reader, tokenURL)

	exchanger, err := source.Exchanger(t.Context(), "catalog", "temporal")
	require.NoError(t, err)
	assert.Equal(t, tokenexchange.DefaultTimeout, exchanger.Timeout())

	exchanged, err := exchanger.Exchange(t.Context(), auth.SubjectToken(subjectToken), credentials.KindStart)
	require.NoError(t, err)
	assert.Equal(t, credentials.Token("exchanged"), exchanged.Token)
	assert.Equal(t, int32(1), received.Load())
}

func TestSourceRefusesAnExchangeItCannotUse(t *testing.T) {
	server, received := countingEndpoint(t)
	tokenURL := server.URL + "/token"

	invalid := checkedExchange("catalog", "invalid", tokenURL)
	meta.SetStatusCondition(&invalid.Status.Conditions, metav1.Condition{
		Type: tokenexchange.ConditionValid, Status: metav1.ConditionFalse, Reason: tokenexchange.ReasonSecretMissing,
		Message: "client secret: Secret `exchange-client` has no key `client-secret`", ObservedGeneration: 1,
	})

	edited := checkedExchange("catalog", "edited", tokenURL)
	edited.Generation = 2

	tests := []struct {
		name        string
		namespace   string
		exchange    string
		allowed     []string
		wantMessage string
	}{
		{name: "exchange only in another namespace", namespace: "catalog", exchange: "elsewhere", allowed: []string{tokenURL},
			wantMessage: "token exchange `elsewhere` doesn't exist in namespace catalog"},
		{name: "missing exchange", namespace: "catalog", exchange: "absent", allowed: []string{tokenURL},
			wantMessage: "token exchange `absent` doesn't exist in namespace catalog"},
		{name: "invalid exchange", namespace: "catalog", exchange: "invalid", allowed: []string{tokenURL},
			wantMessage: "token exchange `invalid` isn't valid: client secret: Secret `exchange-client` has no key `client-secret`"},
		{name: "exchange not checked since its last edit", namespace: "catalog", exchange: "edited", allowed: []string{tokenURL},
			wantMessage: "token exchange `edited` hasn't been checked yet"},
		// The status still says Valid, as it would right after the
		// allow-list changed; the source checks the list itself.
		{name: "tokenURL not on the allow-list", namespace: "catalog", exchange: "temporal", allowed: []string{"https://idp.example/token"},
			wantMessage: "token exchange `temporal`: tokenURL " + tokenURL + " is not on the gateway's allow-list of token endpoints"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
				checkedExchange("catalog", "temporal", tokenURL),
				checkedExchange("other-catalog", "elsewhere", tokenURL),
				invalid, edited,
			).Build()

			_, err := newSource(t, server, reader, test.allowed...).Exchanger(t.Context(), test.namespace, test.exchange)
			require.EqualError(t, err, test.wantMessage)
			assert.Zero(t, received.Load(), "the token endpoint receives nothing")
		})
	}
}

func TestSourceAppliesAnEditToTheNextExchange(t *testing.T) {
	server, _ := countingEndpoint(t)
	tokenURL := server.URL + "/token"

	exchange := checkedExchange("catalog", "temporal", tokenURL)
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(exchange).Build()
	source := newSource(t, server, reader, tokenURL)

	exchanger, err := source.Exchanger(t.Context(), "catalog", "temporal")
	require.NoError(t, err)
	assert.Equal(t, tokenexchange.DefaultTimeout, exchanger.Timeout())

	require.NoError(t, reader.Get(t.Context(), client.ObjectKeyFromObject(exchange), exchange))
	exchange.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Second}
	require.NoError(t, reader.Update(t.Context(), exchange))

	exchanger, err = source.Exchanger(t.Context(), "catalog", "temporal")
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, exchanger.Timeout(), "the next exchanger is built from the edited resource")

	require.NoError(t, reader.Delete(t.Context(), exchange))

	_, err = source.Exchanger(t.Context(), "catalog", "temporal")
	require.EqualError(t, err, "token exchange `temporal` doesn't exist in namespace catalog")
}

func TestSourceClientSecretErrorNamesTheSecretAndKey(t *testing.T) {
	server, received := countingEndpoint(t)
	tokenURL := server.URL + "/token"

	tests := []struct {
		name       string
		key        string
		wantReason string
	}{
		{name: "missing key", key: "absent", wantReason: "Token exchange `temporal` failed: its client secret can't be read: Secret `exchange-client` has no key `absent`"},
		{name: "blank value", key: "blank", wantReason: "Token exchange `temporal` failed: its client secret can't be read: key `blank` of Secret `exchange-client` is empty"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			exchange := checkedExchange("catalog", "temporal", tokenURL)
			exchange.Spec.ClientSecretRef.Key = test.key
			reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(exchange).Build()

			exchanger, err := newSource(t, server, reader, tokenURL).Exchanger(t.Context(), "catalog", "temporal")
			require.NoError(t, err)

			_, err = exchanger.Exchange(t.Context(), auth.SubjectToken(subjectToken), credentials.KindStart)
			require.EqualError(t, err, test.wantReason)
			assert.NotContains(t, err.Error(), secretValue)
			assert.Zero(t, received.Load())
		})
	}
}

func TestLookupAcceptsOnlyAValidExchangeOfTheTemplateNamespace(t *testing.T) {
	invalid := checkedExchange("catalog", "invalid", allowedTokenURL)
	meta.SetStatusCondition(&invalid.Status.Conditions, metav1.Condition{
		Type: tokenexchange.ConditionValid, Status: metav1.ConditionFalse, Reason: tokenexchange.ReasonURLNotAllowed,
		Message: "tokenURL is not on the allow-list", ObservedGeneration: 1,
	})

	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		checkedExchange("catalog", "temporal", allowedTokenURL),
		checkedExchange("other-catalog", "elsewhere", allowedTokenURL),
		invalid,
	).Build()
	lookup := tokenexchange.Lookup{Reader: reader}

	assert.Empty(t, lookup.ExchangeProblem("catalog", "temporal"))
	assert.Equal(t, "token exchange `elsewhere` doesn't exist in namespace catalog", lookup.ExchangeProblem("catalog", "elsewhere"))
	assert.Equal(t, "token exchange `invalid` isn't valid: tokenURL is not on the allow-list", lookup.ExchangeProblem("catalog", "invalid"))
}
