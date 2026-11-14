//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Kubernetes RBAC limits the gateway's reads of TokenExchange resources, and
// its status writes, to the catalog namespaces: a TokenExchange decides where
// the gateway sends users' tokens.
func TestTokenExchangeRBACConfinedToCatalogNamespaces(t *testing.T) {
	admin := adminClient(t)
	user := "system:serviceaccount:" + releaseNamespace() + ":" + gatewayServiceAccount

	can := func(t *testing.T, namespace, verb, subresource string) bool {
		t.Helper()

		review, err := admin.AuthorizationV1().SubjectAccessReviews().Create(t.Context(), &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User: user,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: namespace, Verb: verb,
					Group: "toolgate.soliddowant.dev", Resource: "tokenexchanges", Subresource: subresource,
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		return review.Status.Allowed
	}

	tests := []struct {
		name      string
		namespace string
		allowed   bool
	}{
		// The release namespace is also the catalog namespace here.
		{"catalog namespace", releaseNamespace(), true},
		{"template secrets namespace", templateSecretsNamespace(t), false},
		{"shared namespace", e2eSharedNamespace, false},
		{"another namespace", "default", false},
		{"every namespace", "", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, verb := range []string{"get", "list", "watch"} {
				assert.Equal(t, test.allowed, can(t, test.namespace, verb, ""), "%s tokenexchanges in %q", verb, test.namespace)
			}

			assert.Equal(t, test.allowed, can(t, test.namespace, "update", "status"), "update tokenexchanges/status in %q", test.namespace)

			for _, verb := range []string{"create", "update", "patch", "delete"} {
				assert.False(t, can(t, test.namespace, verb, ""), "%s tokenexchanges in %q", verb, test.namespace)
			}
		})
	}
}
