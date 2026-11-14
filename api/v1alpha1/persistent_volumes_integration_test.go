//go:build integration

package v1alpha1_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestCRDValidatesPersistentVolumes(t *testing.T) {
	environment := testutil.StartEnvtest(t)

	volume := func(change func(*v1alpha1.PersistentVolume)) v1alpha1.PersistentVolume {
		item := v1alpha1.PersistentVolume{Name: "cache", Size: "1Gi", StorageClassName: "ssd"}
		change(&item)

		return item
	}

	tests := []struct {
		name    string
		change  func(*v1alpha1.MCPServerTemplateSpec)
		message string
	}{
		{name: "a per-user template with a volume is accepted", change: func(*v1alpha1.MCPServerTemplateSpec) {}},
		{name: "a name of 63 characters is accepted", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.PersistentVolumes[0].Name = strings.Repeat("a", 63)
		}},
		{name: "a name of 64 characters is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.PersistentVolumes[0].Name = strings.Repeat("a", 64)
		}, message: "spec.persistentVolumes[0].name"},
		{name: "a name with an upper-case letter is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.PersistentVolumes[0].Name = "Cache"
		}, message: "spec.persistentVolumes[0].name"},
		{name: "a size of zero is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.PersistentVolumes[0].Size = "0"
		}, message: "size must be a quantity greater than 0"},
		{name: "a size that is not a quantity is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.PersistentVolumes[0].Size = "big"
		}, message: "size must be a quantity greater than 0"},
		{name: "a scope that is not per-template or per-user is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.PersistentVolumes[0].Scope = "shared"
		}, message: "scope must be one of per-template, per-user"},
		{name: "volumes with remote are refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Endpoint = v1alpha1.Endpoint{}
			spec.Remote = &v1alpha1.RemoteServer{URL: "https://mcp.example.com/mcp"}
		}, message: "persistentVolumes cannot be set together with remote"},
		{name: "volumes in a shared namespace are refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Scope = "shared"
			spec.Namespace = "shared"
		}, message: "persistentVolumes needs namespace: per-instance"},
		{name: "volumes on a per-session template are refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Scope = "per-session"
		}, message: "a per-session template cannot have persistentVolumes"},
		{name: "nine volumes are refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.PersistentVolumes = nil
			for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
				spec.PersistentVolumes = append(spec.PersistentVolumes, volume(func(item *v1alpha1.PersistentVolume) { item.Name = name }))
			}
		}, message: "spec.persistentVolumes"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := &v1alpha1.MCPServerTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "files", Namespace: "default"},
				Spec: v1alpha1.MCPServerTemplateSpec{
					Scope:             "per-user",
					Endpoint:          v1alpha1.Endpoint{Service: "mcp", Port: 8080},
					PersistentVolumes: []v1alpha1.PersistentVolume{volume(func(*v1alpha1.PersistentVolume) {})},
				},
			}
			test.change(&template.Spec)

			err := environment.Client.Create(t.Context(), template, client.DryRunAll)
			if test.message == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.message)
		})
	}
}
