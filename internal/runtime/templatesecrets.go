package runtime

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// APITemplateSecrets is the TemplateSecretSource that reads Secrets in the
// template secrets namespace. Reader must not be a cache (controller-runtime's
// APIReader), so a render never uses an old value and a Secret created after
// a failed start is seen by the next one at once.
type APITemplateSecrets struct {
	Reader    client.Reader
	Namespace string
}

// TemplateSecret implements TemplateSecretSource. Its errors name the Secret
// and the key, never a value.
func (a APITemplateSecrets) TemplateSecret(ctx context.Context, name, key string) ([]byte, string, error) {
	secret := &corev1.Secret{}

	err := a.Reader.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: name}, secret)
	if apierrors.IsNotFound(err) {
		return nil, "", fmt.Errorf("Secret `%s` doesn't exist in `%s`", name, a.Namespace) //nolint:staticcheck // Secret is the kind's name.
	}

	if err != nil {
		return nil, "", fmt.Errorf("the API server refused to read Secret `%s`: %s", name, apiServerMessage(err))
	}

	value, found := secret.Data[key]
	if !found {
		return nil, "", fmt.Errorf("Secret `%s` has no key `%s`", name, key) //nolint:staticcheck // Secret is the kind's name.
	}

	return value, secret.ResourceVersion, nil
}
