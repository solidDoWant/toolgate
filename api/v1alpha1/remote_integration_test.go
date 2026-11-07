//go:build integration

package v1alpha1_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func remoteTemplate(change func(*v1alpha1.MCPServerTemplateSpec)) *v1alpha1.MCPServerTemplate {
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "mail-archiver", Namespace: "default"},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope: "per-user",
			Remote: &v1alpha1.RemoteServer{
				URL:     "http://mail-archiver.email.svc.cluster.local:5000/mcp",
				Headers: map[string]string{"Authorization": "Bearer ${secret:mail-archiver-api-key}"},
			},
		},
	}
	change(&template.Spec)

	return template
}

func TestCRDValidatesRemoteTemplates(t *testing.T) {
	environment := testutil.StartEnvtest(t)

	service := k8sruntime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"mcp"}}`)}
	duration := func(value time.Duration) *metav1.Duration { return &metav1.Duration{Duration: value} }

	tests := []struct {
		name    string
		change  func(*v1alpha1.MCPServerTemplateSpec)
		message string
	}{
		{name: "a remote per-user template is accepted", change: func(*v1alpha1.MCPServerTemplateSpec) {}},
		{name: "a launched template with an endpoint is accepted", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Remote = nil
			spec.Endpoint = v1alpha1.Endpoint{Service: "mcp", Port: 8080}
		}},
		{name: "remote with an endpoint is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Endpoint = v1alpha1.Endpoint{Service: "mcp", Port: 8080}
		}, message: "remote cannot be set together with endpoint, resources or healthCheck"},
		{name: "remote with resources is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Resources = []k8sruntime.RawExtension{service}
		}, message: "remote cannot be set together with endpoint, resources or healthCheck"},
		{name: "remote with a health check is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.HealthCheck = &v1alpha1.HealthCheck{HTTP: v1alpha1.HealthCheckHTTP{Service: "mcp", Port: 8080, Path: "/healthz"}}
		}, message: "remote cannot be set together with endpoint, resources or healthCheck"},
		{name: "remote with scope per-session is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Scope = "per-session"
		}, message: "remote requires scope: per-user"},
		{name: "remote with scope shared is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Scope = "shared"
		}, message: "remote requires scope: per-user"},
		{name: "neither remote nor an endpoint is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Remote = nil
		}, message: "endpoint is required unless remote is set"},
		{name: "a timeout of 1s is accepted", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Remote.Timeout = duration(time.Second)
		}},
		{name: "a timeout of 5m is accepted", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Remote.Timeout = duration(5 * time.Minute)
		}},
		{name: "a timeout under 1s is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Remote.Timeout = duration(999 * time.Millisecond)
		}, message: "timeout must be between 1s and 5m"},
		{name: "a timeout over 5m is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Remote.Timeout = duration(5*time.Minute + time.Second)
		}, message: "timeout must be between 1s and 5m"},
		{name: "a remote without a url is refused", change: func(spec *v1alpha1.MCPServerTemplateSpec) {
			spec.Remote.URL = ""
		}, message: "spec.remote.url"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := environment.Client.Create(t.Context(), remoteTemplate(test.change), client.DryRunAll)
			if test.message == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.message)
		})
	}
}
