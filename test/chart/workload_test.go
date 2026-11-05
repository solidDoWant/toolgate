package chart

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

func renderWith(t *testing.T, set ...string) []manifest {
	t.Helper()

	manifests, err := render(t, append(set, quickStartValues...))
	require.NoError(t, err)

	return manifests
}

func ofKind(manifests []manifest, kind string) []manifest {
	var found []manifest

	for _, candidate := range manifests {
		if candidate.Kind == kind {
			found = append(found, candidate)
		}
	}

	return found
}

func TestPodDisruptionBudgetCoversTheGatewayPods(t *testing.T) {
	manifests := renderQuickStart(t)
	budget := find[policyv1.PodDisruptionBudget](t, manifests, "PodDisruptionBudget")
	deployment := find[appsv1.Deployment](t, manifests, "Deployment")

	require.NotNil(t, budget.Spec.MinAvailable)
	assert.Equal(t, 1, budget.Spec.MinAvailable.IntValue())
	require.NotNil(t, budget.Spec.UnhealthyPodEvictionPolicy)
	assert.Equal(t, policyv1.AlwaysAllow, *budget.Spec.UnhealthyPodEvictionPolicy)

	selector, err := metav1.LabelSelectorAsSelector(budget.Spec.Selector)
	require.NoError(t, err)
	assert.False(t, selector.Empty())
	assert.True(t, selector.Matches(labels.Set(deployment.Spec.Template.Labels)), "the budget selects the gateway pods")
}

func TestMetricsServiceAndServiceMonitor(t *testing.T) {
	manifests := renderQuickStart(t)
	deployment := find[appsv1.Deployment](t, manifests, "Deployment")

	var metrics *corev1.Service

	for _, candidate := range ofKind(manifests, "Service") {
		var service corev1.Service
		require.NoError(t, yaml.UnmarshalStrict(candidate.raw, &service))

		for _, port := range service.Spec.Ports {
			if port.Port == 9090 {
				metrics = &service
			}
		}
	}

	require.NotNil(t, metrics, "a Service exposes the metrics port")
	assert.True(t, labels.SelectorFromSet(metrics.Spec.Selector).Matches(labels.Set(deployment.Spec.Template.Labels)))
	assert.Empty(t, ofKind(manifests, "ServiceMonitor"), "no ServiceMonitor by default")

	monitors := ofKind(renderWith(t, "config.serviceMonitor.enabled=true"), "ServiceMonitor")
	require.Len(t, monitors, 1)

	var monitor struct {
		Spec struct {
			Selector  metav1.LabelSelector `json:"selector"`
			Endpoints []struct {
				Port string `json:"port"`
			} `json:"endpoints"`
		} `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(monitors[0].raw, &monitor))

	selector, err := metav1.LabelSelectorAsSelector(&monitor.Spec.Selector)
	require.NoError(t, err)
	assert.True(t, selector.Matches(labels.Set(metrics.Labels)), "the ServiceMonitor selects the metrics Service")
	require.Len(t, monitor.Spec.Endpoints, 1)
	assert.Equal(t, metrics.Spec.Ports[0].Name, monitor.Spec.Endpoints[0].Port)
}

func TestMemoryRequestEqualsLimitAndNoCPULimit(t *testing.T) {
	deployment := find[appsv1.Deployment](t, renderQuickStart(t), "Deployment")
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	resources := deployment.Spec.Template.Spec.Containers[0].Resources

	request, limit := resources.Requests[corev1.ResourceMemory], resources.Limits[corev1.ResourceMemory]
	assert.False(t, limit.IsZero(), "a memory limit is set")
	assert.Equal(t, 0, request.Cmp(limit), "memory request %s equals limit %s", request.String(), limit.String())
	assert.NotContains(t, resources.Limits, corev1.ResourceCPU)
	assert.NotEqual(t, resource.Quantity{}, resources.Requests[corev1.ResourceCPU], "a CPU request is set")
}

func TestCRDsAreTemplates(t *testing.T) {
	names := []string{
		"mcpservertemplates.toolgate.soliddowant.dev",
		"toolgaterules.toolgate.soliddowant.dev",
		"settingsprofiles.toolgate.soliddowant.dev",
	}

	crds := func(manifests []manifest) map[string]apiextensionsv1.CustomResourceDefinition {
		found := map[string]apiextensionsv1.CustomResourceDefinition{}

		for _, candidate := range ofKind(manifests, "CustomResourceDefinition") {
			var crd apiextensionsv1.CustomResourceDefinition
			require.NoError(t, yaml.UnmarshalStrict(candidate.raw, &crd))
			found[crd.Name] = crd
		}

		return found
	}

	t.Run("rendered and kept by default", func(t *testing.T) {
		rendered := crds(renderQuickStart(t))
		require.Len(t, rendered, len(names))

		for _, name := range names {
			require.Contains(t, rendered, name)
			assert.Equal(t, "keep", rendered[name].Annotations["helm.sh/resource-policy"], name)
		}
	})

	t.Run("not kept", func(t *testing.T) {
		rendered := crds(renderWith(t, "config.crds.keep=false"))
		require.Len(t, rendered, len(names))

		for _, name := range names {
			assert.NotContains(t, rendered[name].Annotations, "helm.sh/resource-policy", name)
		}
	})

	t.Run("not installed", func(t *testing.T) {
		assert.Empty(t, crds(renderWith(t, "config.crds.enabled=false")))
	})
}

// TestValuesCannotChangeTheSecurityBoundary renders the chart with values
// that try to change the gateway's RBAC and admission policy, and checks the
// ClusterRole, Role, their bindings and the admission policy are the same as
// with the quick-start values.
func TestValuesCannotChangeTheSecurityBoundary(t *testing.T) {
	guarded := map[string]bool{
		"ClusterRole": true, "ClusterRoleBinding": true, "Role": true, "RoleBinding": true,
		"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true,
	}

	byName := func(manifests []manifest) map[string][]byte {
		found := map[string][]byte{}

		for _, candidate := range manifests {
			if guarded[candidate.Kind] {
				found[candidate.Kind+"/"+candidate.Metadata.Name] = candidate.raw
			}
		}

		return found
	}

	baseline := byName(renderQuickStart(t))
	require.Len(t, baseline, len(guarded))

	for _, tampering := range [][]string{
		{"rbac.create=false", "rbac.rules[0].apiGroups[0]=*", "clusterRole.rules=null"},
		{"config.rbac.rules[0].verbs[0]=*", "config.admissionPolicy.enabled=false"},
		{"admissionPolicy.failurePolicy=Ignore", "config.admissionPolicy.failurePolicy=Ignore"},
		{"resources.rbac.roles.gateway.type=ClusterRole", "resources.rbac.roles.gateway.rules[0].verbs[0]=*"},
	} {
		t.Run(tampering[0], func(t *testing.T) {
			rendered := byName(renderWith(t, tampering...))
			for name, raw := range baseline {
				if assert.Contains(t, rendered, name) {
					assert.True(t, bytes.Equal(raw, rendered[name]), "%s changed:\n%s", name, rendered[name])
				}
			}
		})
	}
}
