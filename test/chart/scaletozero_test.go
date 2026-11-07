package chart

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const embeddingsDeployment = releaseName + "-embeddings"

// scaleToZeroValues run the embeddings server and let the gateway scale it.
var scaleToZeroValues = []string{"--set", "embeddings.server.enabled=true", "--set", "embeddings.server.scaleToZero.enabled=true"}

func embeddingsDeploymentOf(t *testing.T, manifests []manifest) appsv1.Deployment {
	t.Helper()

	for _, item := range manifests {
		if item.Kind == "Deployment" && item.Metadata.Name == embeddingsDeployment {
			var deployment appsv1.Deployment
			require.NoError(t, yaml.Unmarshal(item.raw, &deployment))

			return deployment
		}
	}

	require.Fail(t, "no embeddings Deployment")

	return appsv1.Deployment{}
}

func gatewayEnv(t *testing.T, manifests []manifest) map[string]string {
	t.Helper()

	env := map[string]string{}

	for _, item := range manifests {
		if item.Kind != "Deployment" || item.Metadata.Name != releaseName {
			continue
		}

		var deployment appsv1.Deployment
		require.NoError(t, yaml.Unmarshal(item.raw, &deployment))

		for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
			env[variable.Name] = variable.Value
		}
	}

	require.NotEmpty(t, env, "the gateway Deployment renders")

	return env
}

func scalerRole(manifests []manifest) (manifest, bool) {
	for _, item := range manifests {
		if item.Kind == "Role" && item.Metadata.Name == embeddingsDeployment+"-scaler" {
			return item, true
		}
	}

	return manifest{}, false
}

func admissionPolicyText(t *testing.T, manifests []manifest) string {
	t.Helper()

	policy := find[admissionv1.ValidatingAdmissionPolicy](t, manifests, "ValidatingAdmissionPolicy")

	var text strings.Builder
	for _, variable := range policy.Spec.Variables {
		text.WriteString(variable.Name + ": " + variable.Expression + "\n")
	}

	for _, validation := range policy.Spec.Validations {
		text.WriteString(validation.Expression + "\n")
	}

	return text.String()
}

func TestEmbeddingsScaleToZeroEnabled(t *testing.T) {
	manifests := renderQuickStart(t, scaleToZeroValues...)

	t.Run("the embeddings Deployment has no replicas field", func(t *testing.T) {
		deployment := embeddingsDeploymentOf(t, manifests)
		assert.Nil(t, deployment.Spec.Replicas)
	})

	t.Run("the gateway gets the Deployment name and the idle timeout", func(t *testing.T) {
		env := gatewayEnv(t, manifests)
		assert.Equal(t, embeddingsDeployment, env["TOOLGATE_TOOL_SEARCH_EMBEDDINGS_SCALE_TO_ZERO_DEPLOYMENT"])
		assert.Equal(t, "15m", env["TOOLGATE_TOOL_SEARCH_EMBEDDINGS_SCALE_TO_ZERO_IDLE_TIMEOUT"])
	})

	t.Run("a Role scales only the embeddings Deployment", func(t *testing.T) {
		item, found := scalerRole(manifests)
		require.True(t, found, "the scaler Role renders")
		assert.Equal(t, releaseNamespace, item.Metadata.Namespace)

		var role rbacv1.Role
		require.NoError(t, yaml.Unmarshal(item.raw, &role))
		assert.Equal(t, []rbacv1.PolicyRule{
			{APIGroups: []string{"apps"}, Resources: []string{"deployments/scale"}, ResourceNames: []string{embeddingsDeployment}, Verbs: []string{"get", "patch"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, ResourceNames: []string{embeddingsDeployment}, Verbs: []string{"get"}},
		}, role.Rules)
	})

	t.Run("a RoleBinding gives the Role to the gateway's service account", func(t *testing.T) {
		var bindings []rbacv1.RoleBinding

		for _, item := range manifests {
			if item.Kind == "RoleBinding" && item.Metadata.Name == embeddingsDeployment+"-scaler" {
				var binding rbacv1.RoleBinding
				require.NoError(t, yaml.Unmarshal(item.raw, &binding))

				bindings = append(bindings, binding)
			}
		}

		require.Len(t, bindings, 1)
		assert.Equal(t, releaseNamespace, bindings[0].Namespace)
		assert.Equal(t, rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: embeddingsDeployment + "-scaler"}, bindings[0].RoleRef)
		assert.Equal(t, []rbacv1.Subject{{Kind: "ServiceAccount", Name: releaseName, Namespace: releaseNamespace}}, bindings[0].Subjects)
	})

	t.Run("the admission policy allows the scale of the embeddings Deployment", func(t *testing.T) {
		text := admissionPolicyText(t, manifests)
		assert.Contains(t, text, `request.name == "`+embeddingsDeployment+`"`)
		assert.Contains(t, text, `variables.subResource == "scale"`)
		assert.Equal(t, 2, strings.Count(text, "|| variables.isEmbeddingsScale"), "both namespace checks allow it")
	})
}

func TestEmbeddingsScaleToZeroDisabled(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "by default", args: []string{"--set", "embeddings.server.enabled=true"}},
		{name: "without the embeddings server", args: []string{"--set", "embeddings.server.scaleToZero.enabled=true"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manifests := renderQuickStart(t, testCase.args...)

			_, found := scalerRole(manifests)
			assert.False(t, found, "no scaler Role")
			assert.NotContains(t, admissionPolicyText(t, manifests), "isEmbeddingsScale")

			env := gatewayEnv(t, manifests)
			assert.NotContains(t, env, "TOOLGATE_TOOL_SEARCH_EMBEDDINGS_SCALE_TO_ZERO_DEPLOYMENT")
			assert.NotContains(t, env, "TOOLGATE_TOOL_SEARCH_EMBEDDINGS_SCALE_TO_ZERO_IDLE_TIMEOUT")

			for _, item := range manifests {
				if item.Kind == "Deployment" && item.Metadata.Name == embeddingsDeployment {
					deployment := embeddingsDeploymentOf(t, manifests)
					require.NotNil(t, deployment.Spec.Replicas)
					assert.Equal(t, int32(1), *deployment.Spec.Replicas)
				}
			}
		})
	}
}

func TestEmbeddingsScaleToZeroIdleTimeout(t *testing.T) {
	cases := []struct {
		timeout      string
		errAssertion require.ErrorAssertionFunc
	}{
		{timeout: "1m"},
		{timeout: "60s"},
		{timeout: "24h"},
		{timeout: "1h30m"},
		{timeout: "59s", errAssertion: refusedAtTemplateTime("from 1m to 24h")},
		{timeout: "24h1s", errAssertion: refusedAtTemplateTime("from 1m to 24h")},
		{timeout: "0s", errAssertion: refusedAtTemplateTime("from 1m to 24h")},
		{timeout: "15", errAssertion: refusedAtTemplateTime("must be a duration")},
		{timeout: "fifteen", errAssertion: refusedAtTemplateTime("must be a duration")},
		{timeout: "1.5h", errAssertion: refusedAtTemplateTime("must be a duration")},
	}

	for _, testCase := range cases {
		t.Run(testCase.timeout, func(t *testing.T) {
			if testCase.errAssertion == nil {
				testCase.errAssertion = require.NoError
			}

			set := append([]string{
				"embeddings.server.enabled=true", "embeddings.server.scaleToZero.enabled=true",
				"embeddings.server.scaleToZero.idleTimeout=" + testCase.timeout,
			}, quickStartValues...)
			manifests, err := render(t, set)
			testCase.errAssertion(t, err)

			if err == nil {
				assert.Equal(t, testCase.timeout, gatewayEnv(t, manifests)["TOOLGATE_TOOL_SEARCH_EMBEDDINGS_SCALE_TO_ZERO_IDLE_TIMEOUT"])
			}
		})
	}
}

func TestEmbeddingsScaleToZeroRefusesAHandSetURL(t *testing.T) {
	set := append([]string{
		"embeddings.server.enabled=true", "embeddings.server.scaleToZero.enabled=true",
		"config.toolSearchEmbeddings.url=http://external:80",
	}, quickStartValues...)
	_, err := render(t, set)
	refusedAtTemplateTime("can't be used with config.toolSearchEmbeddings.url")(t, err)
}

func refusedAtTemplateTime(message string) require.ErrorAssertionFunc {
	return func(t require.TestingT, err error, _ ...any) {
		require.Error(t, err)
		assert.Contains(t, err.Error(), message)
	}
}

func TestOwnerDetailsOnInstanceObjectsCanBeTurnedOff(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "on by default", want: "false"},
		{name: "turned off", args: []string{"--set", "config.instanceOwnerDetails=false"}, want: "true"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := gatewayEnv(t, renderQuickStart(t, test.args...))
			assert.Equal(t, test.want, env["TOOLGATE_INSTANCE_OMIT_OWNER_DETAILS"])
		})
	}
}
