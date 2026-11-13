package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// orderForApply returns objects in the order they are applied: every Secret
// first, then the other objects, each group in template order. A Secret of
// type kubernetes.io/service-account-token keeps its template position among
// the other objects, because it names a ServiceAccount that must exist before
// Kubernetes fills the Secret.
func orderForApply(objects []*unstructured.Unstructured) []*unstructured.Unstructured {
	ordered := make([]*unstructured.Unstructured, 0, len(objects))

	var rest []*unstructured.Unstructured

	for _, object := range objects {
		if isPlainSecret(object) {
			ordered = append(ordered, object)
		} else {
			rest = append(rest, object)
		}
	}

	return append(ordered, rest...)
}

func isPlainSecret(object *unstructured.Unstructured) bool {
	if object.GetKind() != "Secret" || object.GetAPIVersion() != "v1" {
		return false
	}

	secretType, _, _ := unstructured.NestedString(object.Object, "type")

	return secretType != string(corev1.SecretTypeServiceAccountToken)
}

// SecretKeyValue is the new value of one key of a Secret.
type SecretKeyValue struct {
	Namespace string
	Secret    string
	Key       string
	Value     []byte
}

// PatchSecretKeys updates keys of Secrets in place. It sends one JSON merge
// patch, {"data": {key: base64}}, for each Secret with all of its changed
// keys, with field manager toolgate. It never reads a Secret: each patch
// carries the full value of its keys. It patches every Secret it can and
// returns the first error.
func PatchSecretKeys(ctx context.Context, patcher client.Writer, values []SecretKeyValue) error {
	type secretID struct{ namespace, name string }

	byID := map[secretID]map[string][]byte{}

	var order []secretID

	for _, value := range values {
		id := secretID{value.Namespace, value.Secret}
		if byID[id] == nil {
			byID[id] = map[string][]byte{}
			order = append(order, id)
		}

		byID[id][value.Key] = value.Value
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i].namespace != order[j].namespace {
			return order[i].namespace < order[j].namespace
		}

		return order[i].name < order[j].name
	})

	var first error

	for _, id := range order {
		// []byte values encode as standard base64 in JSON.
		body, err := json.Marshal(map[string]any{"data": byID[id]})
		if err != nil {
			return fmt.Errorf("encode patch of Secret %s/%s: %w", id.namespace, id.name, err)
		}

		secret := &corev1.Secret{}
		secret.Namespace, secret.Name = id.namespace, id.name

		err = patcher.Patch(ctx, secret, client.RawPatch(types.MergePatchType, body), client.FieldOwner(FieldManager))
		if err != nil && first == nil {
			first = fmt.Errorf("patch Secret %s/%s: %w", id.namespace, id.name, err)
		}
	}

	return first
}
