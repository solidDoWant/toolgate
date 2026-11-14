package catalog_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/templatescope"
)

func TestValidatePersistentVolumes(t *testing.T) {
	const (
		pod     = `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"mcp"}}`
		claim   = `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"cache"}}`
		otherPV = `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"other"}}`
	)

	tests := []struct {
		name      string
		scope     string
		volume    string // the volume's scope
		resources []string
		disabled  bool
		change    func(*v1alpha1.MCPServerTemplate)
		want      []string
		notWant   string
	}{
		{name: "per-user template, unset volume scope", scope: "per-user"},
		{name: "per-user template, per-user volume", scope: "per-user", volume: "per-user"},
		{name: "per-user template, per-template volume", scope: "per-user", volume: "per-template",
			want: []string{"persistentVolumes `cache`: scope `per-template` is wider than the template's effective scope `per-user`"}},
		{name: "shared template, unset volume scope", scope: "shared"},
		{name: "shared template, per-template volume", scope: "shared", volume: "per-template"},
		{name: "shared template, per-user volume", scope: "shared", volume: "per-user",
			want: []string{"persistentVolumes `cache`: scope `per-user` needs a user, and a shared template has none"}},
		{name: "derived per-session template, per-user volume", scope: "", volume: "per-user",
			change: func(template *v1alpha1.MCPServerTemplate) {
				template.Spec.Resources = []runtime.RawExtension{{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{"s":"${session.id}"}}`)}}
			},
			want: []string{"persistentVolumes `cache`: scope `per-user` is wider than the template's effective scope `per-session`"}},
		{name: "per-session template", scope: "per-session",
			want: []string{"a per-session template cannot have persistentVolumes"}},
		{name: "feature off", scope: "per-user", disabled: true,
			want: []string{"persistentVolumes needs the gateway setting persistentVolumes.enabled"}},
		{name: "remote", scope: "per-user", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Endpoint = v1alpha1.Endpoint{}
			template.Spec.Remote = &v1alpha1.RemoteServer{URL: "https://mcp.example.com/mcp"}
		}, want: []string{"persistentVolumes cannot be set together with remote"}},
		{name: "shared namespace", scope: "shared", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.Namespace = catalog.NamespaceShared
			template.Spec.Endpoint.Service = "mcp-${instance.id}"
		}, want: []string{"persistentVolumes needs namespace: per-instance"}},
		{name: "size zero", scope: "per-user", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.PersistentVolumes[0].Size = "0"
		}, want: []string{"persistentVolumes `cache`: size must be a quantity greater than 0"}},
		{name: "size not a quantity", scope: "per-user", change: func(template *v1alpha1.MCPServerTemplate) {
			template.Spec.PersistentVolumes[0].Size = "big"
		}, want: []string{"persistentVolumes `cache`: size must be a quantity greater than 0"}},
		{name: "pod in resources", scope: "per-user", resources: []string{pod},
			want: []string{"persistentVolumes cannot be used with a Pod in resources. Use a Deployment or StatefulSet."}},
		{name: "claim with the volume's name", scope: "per-user", resources: []string{serviceResource, claim},
			want: []string{"resources[1] is a PersistentVolumeClaim named `cache`, which persistentVolumes already makes"}},
		{name: "claim with another name", scope: "per-user", resources: []string{otherPV}, notWant: "which persistentVolumes already makes"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := newTemplate("files", test.scope, test.resources...)
			template.Spec.PersistentVolumes = []v1alpha1.PersistentVolume{{Name: "cache", Size: "1Gi", StorageClassName: "ssd", Scope: test.volume}}

			if test.change != nil {
				test.change(template)
			}

			problems := catalog.Validator{PersistentVolumes: !test.disabled}.Validate(template)

			if test.want == nil {
				if test.notWant != "" {
					assert.NotContains(t, catalog.Reason(problems), test.notWant)

					return
				}

				assert.Empty(t, problems)

				return
			}

			for _, want := range test.want {
				assert.Contains(t, problems, want)
			}
		})
	}
}

func TestValidateWithoutVolumesIgnoresTheSetting(t *testing.T) {
	assert.Empty(t, catalog.Validator{}.Validate(newTemplate("files", "per-user")))
}

func TestEffectiveVolumeScope(t *testing.T) {
	tests := []struct {
		scope, volume, want string
	}{
		{"shared", "", "per-template"},
		{"per-user", "", "per-user"},
		{"per-session", "", ""},
		{"per-user", "per-template", "per-template"},
	}

	for _, test := range tests {
		spec := v1alpha1.MCPServerTemplateSpec{Scope: test.scope}
		require.Equal(t, test.want, templatescope.EffectiveVolumeScope(spec, v1alpha1.PersistentVolume{Scope: test.volume}))
	}
}
