//go:build integration

package tokenexchange_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/tokenexchange"
)

func createNamespace(t *testing.T, kubeClient client.Client, name string) {
	t.Helper()

	require.NoError(t, kubeClient.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
}

func eventuallyCondition(t *testing.T, kubeClient client.Client, namespace, name, wantReason string, wantMessages ...string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var exchange v1alpha1.TokenExchange
		require.NoError(collect, kubeClient.Get(t.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &exchange))

		condition := meta.FindStatusCondition(exchange.Status.Conditions, tokenexchange.ConditionValid)
		require.NotNil(collect, condition)
		assert.Equal(collect, wantReason, condition.Reason)
		assert.Equal(collect, wantReason == tokenexchange.ReasonValid, condition.Status == metav1.ConditionTrue)
		assert.Equal(collect, exchange.Generation, condition.ObservedGeneration)
		assert.NotContains(collect, condition.Message, secretValue)

		for _, want := range wantMessages {
			assert.Contains(collect, condition.Message, want)
		}
	}, 20*time.Second, 50*time.Millisecond)
}

func TestReconcilerWritesTheValidConditionThroughTheAPIServer(t *testing.T) {
	environment := testutil.StartEnvtest(t)
	kubeClient := environment.Client

	createNamespace(t, kubeClient, "catalog")
	createNamespace(t, kubeClient, secretsNamespace)
	createNamespace(t, kubeClient, "elsewhere")

	require.NoError(t, kubeClient.Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: secretsNamespace, Name: "exchange-client"},
		Data:       map[string][]byte{"client-secret": []byte(secretValue), "empty": {}},
	}))

	mgr, err := ctrl.NewManager(environment.Config, ctrl.Options{
		Scheme:     environment.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&v1alpha1.TokenExchange{}: {Namespaces: map[string]cache.Config{"catalog": {}}},
		}},
	})
	require.NoError(t, err)

	reconciler := &tokenexchange.Reconciler{
		Client:     mgr.GetClient(),
		Namespaces: []string{"catalog"},
		Checker: tokenexchange.Checker{
			AllowList: tokenexchange.StaticAllowList{allowedTokenURL},
			Secrets:   runtime.APITemplateSecrets{Reader: mgr.GetAPIReader(), Namespace: secretsNamespace},
			HasKeyset: true,
		},
		RecheckInterval: 200 * time.Millisecond,
	}
	require.NoError(t, reconciler.SetupWithManager(mgr))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- mgr.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})

	create := func(t *testing.T, name string, change func(spec *v1alpha1.TokenExchangeSpec)) {
		t.Helper()

		exchange := namedExchange("catalog", name)
		if change != nil {
			change(&exchange.Spec)
		}

		require.NoError(t, kubeClient.Create(t.Context(), exchange))
	}

	t.Run("valid", func(t *testing.T) {
		create(t, "valid", nil)
		eventuallyCondition(t, kubeClient, "catalog", "valid", tokenexchange.ReasonValid)
	})

	t.Run("tokenURL not on the allow-list", func(t *testing.T) {
		create(t, "not-allowed", func(spec *v1alpha1.TokenExchangeSpec) { spec.TokenURL = "https://collector.example/token" })
		eventuallyCondition(t, kubeClient, "catalog", "not-allowed", tokenexchange.ReasonURLNotAllowed, "https://collector.example/token")
	})

	t.Run("http tokenURL", func(t *testing.T) {
		create(t, "bad-fields", func(spec *v1alpha1.TokenExchangeSpec) { spec.TokenURL = "http://idp.example/token" })
		eventuallyCondition(t, kubeClient, "catalog", "bad-fields", tokenexchange.ReasonBadFields, "tokenURL")
	})

	t.Run("empty client secret", func(t *testing.T) {
		create(t, "empty-secret", func(spec *v1alpha1.TokenExchangeSpec) { spec.ClientSecretRef.Key = "empty" })
		eventuallyCondition(t, kubeClient, "catalog", "empty-secret", tokenexchange.ReasonSecretMissing, "`exchange-client`", "`empty`")
	})

	t.Run("missing Secret becomes valid once created", func(t *testing.T) {
		create(t, "later-secret", func(spec *v1alpha1.TokenExchangeSpec) { spec.ClientSecretRef.Name = "later-client" })
		eventuallyCondition(t, kubeClient, "catalog", "later-secret", tokenexchange.ReasonSecretMissing, "`later-client`", secretsNamespace)

		require.NoError(t, kubeClient.Create(t.Context(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: secretsNamespace, Name: "later-client"},
			Data:       map[string][]byte{"client-secret": []byte(secretValue)},
		}))
		eventuallyCondition(t, kubeClient, "catalog", "later-secret", tokenexchange.ReasonValid)
	})

	t.Run("an edit is checked again", func(t *testing.T) {
		var exchange v1alpha1.TokenExchange
		require.NoError(t, kubeClient.Get(t.Context(), types.NamespacedName{Namespace: "catalog", Name: "valid"}, &exchange))

		exchange.Spec.TokenURL = "https://collector.example/token"
		require.NoError(t, kubeClient.Update(t.Context(), &exchange))
		eventuallyCondition(t, kubeClient, "catalog", "valid", tokenexchange.ReasonURLNotAllowed)
	})

	t.Run("past the namespace limit", func(t *testing.T) {
		var list v1alpha1.TokenExchangeList
		require.NoError(t, kubeClient.List(t.Context(), &list, client.InNamespace("catalog")))

		// Names that sort after every earlier one, up to one past the limit.
		for index := len(list.Items); index <= tokenexchange.MaxPerNamespace; index++ {
			create(t, fmt.Sprintf("z-%02d", index), nil)
		}

		eventuallyCondition(t, kubeClient, "catalog", fmt.Sprintf("z-%02d", tokenexchange.MaxPerNamespace), tokenexchange.ReasonTooMany, "16")
		eventuallyCondition(t, kubeClient, "catalog", fmt.Sprintf("z-%02d", tokenexchange.MaxPerNamespace-1), tokenexchange.ReasonValid)
	})

	t.Run("other namespaces are not touched", func(t *testing.T) {
		require.NoError(t, kubeClient.Create(t.Context(), namedExchange("elsewhere", "ignored")))

		// The catalog namespace is checked again after the create, so by
		// then the other namespace would have been seen if it were watched.
		eventuallyCondition(t, kubeClient, "catalog", "valid", tokenexchange.ReasonURLNotAllowed)

		var exchange v1alpha1.TokenExchange
		require.NoError(t, kubeClient.Get(t.Context(), types.NamespacedName{Namespace: "elsewhere", Name: "ignored"}, &exchange))
		assert.Empty(t, exchange.Status.Conditions)
	})
}

func TestReconcilerReportsNoKeysetThroughTheAPIServer(t *testing.T) {
	environment := testutil.StartEnvtest(t)
	kubeClient := environment.Client

	createNamespace(t, kubeClient, "catalog")
	require.NoError(t, kubeClient.Create(t.Context(), namedExchange("catalog", "temporal")))

	reconciler := &tokenexchange.Reconciler{
		Client:     kubeClient,
		Namespaces: []string{"catalog"},
		Checker:    tokenexchange.Checker{AllowList: tokenexchange.StaticAllowList{allowedTokenURL}},
	}

	_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "catalog"}})
	require.NoError(t, err)

	eventuallyCondition(t, kubeClient, "catalog", "temporal", tokenexchange.ReasonNoKeyset, "secretsKeysetFile")
}
