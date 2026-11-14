package tokenexchange_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/tokenexchange"
)

func namedExchange(namespace, name string) *v1alpha1.TokenExchange {
	exchange := validExchange()
	exchange.Namespace, exchange.Name = namespace, name

	return exchange
}

func validCondition(t *testing.T, reader client.Reader, namespace, name string) *metav1.Condition {
	t.Helper()

	var exchange v1alpha1.TokenExchange
	require.NoError(t, reader.Get(t.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &exchange))

	return meta.FindStatusCondition(exchange.Status.Conditions, tokenexchange.ConditionValid)
}

func TestReconcilerLimitsEachNamespaceBySortedName(t *testing.T) {
	tests := []struct {
		name        string
		count       int
		wantTooMany []string
	}{
		{name: "sixteen exchanges are all valid", count: 16},
		{name: "the seventeenth by name is too many", count: 17, wantTooMany: []string{"exchange-16"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme(t)).WithStatusSubresource(&v1alpha1.TokenExchange{})

			// Created in reverse, so creation order can't decide which is past
			// the limit.
			for index := test.count - 1; index >= 0; index-- {
				builder.WithObjects(namedExchange("catalog", fmt.Sprintf("exchange-%02d", index)))
			}

			kubeClient := builder.Build()
			reconciler := &tokenexchange.Reconciler{
				Client:     kubeClient,
				Namespaces: []string{"catalog"},
				Checker: tokenexchange.Checker{
					AllowList: tokenexchange.StaticAllowList{allowedTokenURL},
					Secrets:   clientSecrets(t),
					HasKeyset: true,
				},
			}

			result, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "catalog"}})
			require.NoError(t, err)
			assert.Equal(t, tokenexchange.DefaultRecheckInterval, result.RequeueAfter, "the reconciler checks client Secrets again later")

			for index := range test.count {
				name := fmt.Sprintf("exchange-%02d", index)
				condition := validCondition(t, kubeClient, "catalog", name)
				require.NotNil(t, condition, name)

				if index >= tokenexchange.MaxPerNamespace {
					assert.Equal(t, metav1.ConditionFalse, condition.Status, name)
					assert.Equal(t, tokenexchange.ReasonTooMany, condition.Reason, name)
					assert.Contains(t, test.wantTooMany, name)

					continue
				}

				assert.Equal(t, metav1.ConditionTrue, condition.Status, name)
				assert.Equal(t, tokenexchange.ReasonValid, condition.Reason, name)
			}
		})
	}
}

func TestReconcilerIgnoresOtherNamespaces(t *testing.T) {
	kubeClient := fake.NewClientBuilder().WithScheme(scheme(t)).WithStatusSubresource(&v1alpha1.TokenExchange{}).
		WithObjects(namedExchange("elsewhere", "temporal")).Build()

	reconciler := &tokenexchange.Reconciler{
		Client:     kubeClient,
		Namespaces: []string{"catalog"},
		Checker:    tokenexchange.Checker{AllowList: tokenexchange.StaticAllowList{allowedTokenURL}, Secrets: clientSecrets(t), HasKeyset: true},
	}

	_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "elsewhere"}})
	require.NoError(t, err)

	assert.Nil(t, validCondition(t, kubeClient, "elsewhere", "temporal"))
}
