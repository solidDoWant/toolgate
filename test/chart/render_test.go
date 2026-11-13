// Package chart checks the manifests the Helm chart in deploy/charts/toolgate
// renders, without a cluster. `make chart-lint` runs these tests.
package chart

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	apimachineryyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	releaseName      = "toolgate"
	releaseNamespace = "toolgate-system"
	serviceAccount   = "system:serviceaccount:toolgate-system:toolgate"
)

var chartDir = filepath.Join("..", "..", "deploy", "charts", "toolgate")

// quickStartValues are the values docs/admin/quick-start.md installs with.
var quickStartValues = []string{
	"config.database.existingSecret=toolgate-database",
	"config.oidc.issuerURL=https://idp.example.com/realms/main",
	"config.oidc.audience=toolgate",
}

// TestMain fetches the bjw-s common library chart the chart depends on, at
// the version Chart.lock pins (scripts/chart-deps.sh), when helm is installed.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("helm"); err == nil {
		output, err := exec.Command(filepath.Join("..", "..", "scripts", "chart-deps.sh")).CombinedOutput()
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "scripts/chart-deps.sh: %v\n%s", err, output)

			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}

type manifest struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	raw []byte
}

func render(t *testing.T, set []string, extraArgs ...string) ([]manifest, error) {
	t.Helper()

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}

	args := []string{"template", releaseName, chartDir, "--namespace", releaseNamespace}
	for _, value := range set {
		args = append(args, "--set", value)
	}

	args = append(args, extraArgs...)

	var stdout, stderr bytes.Buffer

	command := exec.CommandContext(t.Context(), helm, args...)
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		return nil, errors.New(stderr.String())
	}

	reader := apimachineryyaml.NewYAMLReader(bufio.NewReader(&stdout))

	var manifests []manifest

	for {
		document, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		var parsed manifest
		require.NoError(t, yaml.Unmarshal(document, &parsed))

		if parsed.Kind == "" {
			continue
		}

		parsed.raw = document
		manifests = append(manifests, parsed)
	}

	return manifests, nil
}

func renderQuickStart(t *testing.T, extraArgs ...string) []manifest {
	t.Helper()

	manifests, err := render(t, quickStartValues, extraArgs...)
	require.NoError(t, err)

	return manifests
}

func find[T any](t *testing.T, manifests []manifest, kind string) T {
	t.Helper()

	var found []manifest

	for _, candidate := range manifests {
		if candidate.Kind == kind {
			found = append(found, candidate)
		}
	}

	require.Len(t, found, 1, "exactly one %s", kind)

	var object T
	require.NoError(t, yaml.UnmarshalStrict(found[0].raw, &object))

	return object
}

func TestChartRendersTheGateway(t *testing.T) {
	manifests := renderQuickStart(t)

	var crds []string

	for _, candidate := range manifests {
		if candidate.Kind == "CustomResourceDefinition" {
			crds = append(crds, candidate.Metadata.Name)
		}
	}

	assert.ElementsMatch(t, []string{
		"mcpservertemplates.toolgate.soliddowant.dev",
		"toolgaterules.toolgate.soliddowant.dev",
		"settingsprofiles.toolgate.soliddowant.dev",
	}, crds)

	deployment := find[appsv1.Deployment](t, manifests, "Deployment")
	require.NotNil(t, deployment.Spec.Replicas)
	assert.Equal(t, int32(2), *deployment.Spec.Replicas)
	assert.Equal(t, "toolgate", deployment.Spec.Template.Spec.ServiceAccountName)

	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	container := deployment.Spec.Template.Spec.Containers[0]
	assert.Equal(t, []string{"serve"}, container.Args)
	require.NotNil(t, container.LivenessProbe)
	assert.Equal(t, "/healthz", container.LivenessProbe.HTTPGet.Path)
	require.NotNil(t, container.ReadinessProbe)
	assert.Equal(t, "/readyz", container.ReadinessProbe.HTTPGet.Path)

	var databaseURL *corev1.EnvVar

	for index := range container.Env {
		if container.Env[index].Name == "TOOLGATE_DATABASE_URL" {
			databaseURL = &container.Env[index]
		}
	}

	require.NotNil(t, databaseURL, "TOOLGATE_DATABASE_URL is set")
	require.NotNil(t, databaseURL.ValueFrom)
	require.NotNil(t, databaseURL.ValueFrom.SecretKeyRef)
	assert.Equal(t, "toolgate-database", databaseURL.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "url", databaseURL.ValueFrom.SecretKeyRef.Key)

	values := map[string]string{}
	for _, variable := range container.Env {
		values[variable.Name] = variable.Value
	}

	assert.Equal(t, "https://idp.example.com/realms/main", values["TOOLGATE_OIDC_ISSUER_URL"])
	assert.Equal(t, "toolgate", values["TOOLGATE_OIDC_AUDIENCE"])
	assert.Equal(t, releaseNamespace, values["TOOLGATE_LEASE_NAMESPACE"], "the leader Lease lives in the release namespace")
	assert.NotContains(t, values, "TOOLGATE_OIDC_EMAIL_CLAIM", "an empty claim value keeps the gateway's default")
	assert.NotContains(t, values, "TOOLGATE_CATALOG_NAMESPACES", "no catalog namespaces keeps the gateway's default")
	assert.NotContains(t, values, "TOOLGATE_SHARED_NAMESPACE", "no shared namespace by default")

	podLabels, err := labels.ConvertSelectorToLabelsMap(values["TOOLGATE_GATEWAY_POD_LABELS"])
	require.NoError(t, err)
	require.NotEmpty(t, podLabels)
	assert.True(t, labels.SelectorFromSet(podLabels).Matches(labels.Set(deployment.Spec.Template.Labels)),
		"the gateway pod labels select the gateway's pods")

	for _, candidate := range manifests {
		if candidate.Kind == "CustomResourceDefinition" {
			continue
		}

		assert.NotEqual(t, "toolgate", candidate.Metadata.Labels["app.kubernetes.io/managed-by"],
			"%s %s must not carry toolgate's managed-by label, or the gateway would treat it as its own",
			candidate.Kind, candidate.Metadata.Name)
	}
}

func TestChartRequiresTheDatabaseAndTheIdentityProvider(t *testing.T) {
	for _, required := range []string{"config.database.existingSecret", "config.oidc.issuerURL", "config.oidc.audience"} {
		t.Run(required, func(t *testing.T) {
			var set []string

			for _, value := range quickStartValues {
				if !strings.HasPrefix(value, required+"=") {
					set = append(set, value)
				}
			}

			_, err := render(t, set)
			require.Error(t, err)
			assert.Contains(t, err.Error(), required+" is required")
		})
	}
}

func TestClusterRoleIsExactlyTheDesignedOne(t *testing.T) {
	manifests := renderQuickStart(t)
	role := find[rbacv1.ClusterRole](t, manifests, "ClusterRole")

	write := []string{"create", "patch", "delete", "get", "list", "watch"}
	expected := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"create", "delete", "get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods", "services", "configmaps", "serviceaccounts", "persistentvolumeclaims"}, Verbs: write},
		{APIGroups: []string{"apps"}, Resources: []string{"deployments", "statefulsets"}, Verbs: write},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: write},
		{APIGroups: []string{"cilium.io"}, Resources: []string{"ciliumnetworkpolicies"}, Verbs: write},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create", "patch", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
		{
			APIGroups: []string{"toolgate.soliddowant.dev"},
			Resources: []string{"mcpservertemplates", "toolgaterules", "settingsprofiles"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"toolgate.soliddowant.dev"},
			Resources: []string{"mcpservertemplates/status", "toolgaterules/status", "settingsprofiles/status"},
			Verbs:     []string{"update", "patch"},
		},
	}
	assert.Equal(t, expected, role.Rules)

	binding := find[rbacv1.ClusterRoleBinding](t, manifests, "ClusterRoleBinding")
	assert.Equal(t, role.Name, binding.RoleRef.Name)
	assert.Equal(t, []rbacv1.Subject{{Kind: "ServiceAccount", Name: "toolgate", Namespace: releaseNamespace}}, binding.Subjects)
}

func TestClusterRoleAddsExtraRules(t *testing.T) {
	defaultRole := find[rbacv1.ClusterRole](t, renderQuickStart(t), "ClusterRole")

	manifests, err := render(t, append([]string{
		"rbac.extraRules[0].apiGroups[0]=helm.toolkit.fluxcd.io",
		"rbac.extraRules[0].resources[0]=helmreleases",
		"rbac.extraRules[0].verbs[0]=create",
		"rbac.extraRules[0].verbs[1]=get",
	}, quickStartValues...))
	require.NoError(t, err)

	role := find[rbacv1.ClusterRole](t, manifests, "ClusterRole")

	expected := append(defaultRole.Rules, rbacv1.PolicyRule{
		APIGroups: []string{"helm.toolkit.fluxcd.io"}, Resources: []string{"helmreleases"}, Verbs: []string{"create", "get"},
	})
	assert.Equal(t, expected, role.Rules)
}

func TestRoleCoversOnlyTheReleaseNamespace(t *testing.T) {
	manifests := renderQuickStart(t)
	role := find[rbacv1.Role](t, manifests, "Role")
	assert.Equal(t, releaseNamespace, role.Namespace)
	assert.Equal(t, []rbacv1.PolicyRule{
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create", "get", "list", "watch", "update", "patch", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch"}},
	}, role.Rules)

	binding := find[rbacv1.RoleBinding](t, manifests, "RoleBinding")
	assert.Equal(t, releaseNamespace, binding.Namespace)
	assert.Equal(t, role.Name, binding.RoleRef.Name)
}

func TestAdmissionPolicyAppliesToEveryWriteByTheGateway(t *testing.T) {
	manifests := renderQuickStart(t)
	policy := find[admissionv1.ValidatingAdmissionPolicy](t, manifests, "ValidatingAdmissionPolicy")

	require.NotNil(t, policy.Spec.FailurePolicy)
	assert.Equal(t, admissionv1.Fail, *policy.Spec.FailurePolicy)

	require.NotNil(t, policy.Spec.MatchConstraints)
	require.Len(t, policy.Spec.MatchConstraints.ResourceRules, 1)
	rule := policy.Spec.MatchConstraints.ResourceRules[0].RuleWithOperations
	assert.ElementsMatch(t, []admissionv1.OperationType{admissionv1.Create, admissionv1.Update, admissionv1.Delete}, rule.Operations)
	assert.Equal(t, []string{"*"}, rule.APIGroups)
	assert.Equal(t, []string{"*/*"}, rule.Resources)

	require.Len(t, policy.Spec.MatchConditions, 1)
	assert.Equal(t, `request.userInfo.username == "`+serviceAccount+`"`, policy.Spec.MatchConditions[0].Expression)
	assert.Len(t, policy.Spec.Validations, 5)

	for _, validation := range policy.Spec.Validations {
		assert.NotContains(t, validation.Expression, "isAllowedKind", "RBAC, not the policy, decides which kinds the gateway writes")
	}

	binding := find[admissionv1.ValidatingAdmissionPolicyBinding](t, manifests, "ValidatingAdmissionPolicyBinding")
	assert.Equal(t, policy.Name, binding.Spec.PolicyName)
	assert.Equal(t, []admissionv1.ValidationAction{admissionv1.Deny}, binding.Spec.ValidationActions)
	assert.Nil(t, binding.Spec.MatchResources, "the binding narrows nothing")
}

func TestSharedNamespace(t *testing.T) {
	t.Run("none by default", func(t *testing.T) {
		manifests := renderQuickStart(t)
		for _, candidate := range manifests {
			assert.NotEqual(t, "Namespace", candidate.Kind)
		}
	})

	t.Run("labelled as toolgate's", func(t *testing.T) {
		manifests, err := render(t, append([]string{"config.sharedNamespace.name=toolgate-shared"}, quickStartValues...))
		require.NoError(t, err)

		namespace := find[corev1.Namespace](t, manifests, "Namespace")
		assert.Equal(t, "toolgate-shared", namespace.Name)
		assert.Equal(t, "true", namespace.Labels["toolgate.soliddowant.dev/writable"],
			"the admission policy lets the gateway write only in namespaces with this label")
		assert.Equal(t, "restricted", namespace.Labels["pod-security.kubernetes.io/enforce"])
		assert.NotContains(t, namespace.Labels, "toolgate.soliddowant.dev/instance",
			"the admission policy lets the gateway delete only namespaces with this label")

		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		assert.Contains(t, deployment.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "TOOLGATE_SHARED_NAMESPACE", Value: "toolgate-shared"},
			"the gateway learns the shared namespace's name")
	})

	t.Run("passed to the gateway when the namespace exists already", func(t *testing.T) {
		manifests, err := render(t, append([]string{
			"config.sharedNamespace.name=toolgate-shared", "config.sharedNamespace.create=false",
		}, quickStartValues...))
		require.NoError(t, err)

		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		assert.Contains(t, deployment.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "TOOLGATE_SHARED_NAMESPACE", Value: "toolgate-shared"})
	})

	t.Run("not an instance namespace name", func(t *testing.T) {
		_, err := render(t, append([]string{"config.sharedNamespace.name=tg-shared"}, quickStartValues...))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not start with tg-")
	})
}

func TestInstanceLimits(t *testing.T) {
	gatewayEnv := func(t *testing.T, set ...string) map[string]string {
		t.Helper()

		manifests, err := render(t, append(append([]string{}, quickStartValues...), set...))
		require.NoError(t, err)

		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		values := map[string]string{}

		for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
			values[variable.Name] = variable.Value
		}

		return values
	}

	t.Run("the defaults are 20 per user and 200 in total", func(t *testing.T) {
		values := gatewayEnv(t)
		assert.Equal(t, "20", values["TOOLGATE_INSTANCE_LIMIT_PER_USER"])
		assert.Equal(t, "200", values["TOOLGATE_INSTANCE_LIMIT"])
	})

	t.Run("1 is allowed", func(t *testing.T) {
		values := gatewayEnv(t, "config.limits.instancesPerUser=1", "config.limits.instances=1")
		assert.Equal(t, "1", values["TOOLGATE_INSTANCE_LIMIT_PER_USER"])
		assert.Equal(t, "1", values["TOOLGATE_INSTANCE_LIMIT"])
	})

	for _, setting := range []string{"instancesPerUser", "instances"} {
		for _, value := range []string{"0", "-1", "1.5", "many"} {
			t.Run(setting+"="+value+" is rejected", func(t *testing.T) {
				_, err := render(t, append(append([]string{}, quickStartValues...), "config.limits."+setting+"="+value))
				require.Error(t, err)
				assert.Contains(t, err.Error(), "config.limits."+setting+" must be a whole number of at least 1")
			})
		}
	}
}

func TestOIDCAudienceSetting(t *testing.T) {
	gatewayEnv := func(t *testing.T, extraArgs ...string) map[string]string {
		t.Helper()

		manifests, err := render(t, quickStartValues, extraArgs...)
		require.NoError(t, err)

		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		values := map[string]string{}

		for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
			values[variable.Name] = variable.Value
		}

		return values
	}

	t.Run("one string", func(t *testing.T) {
		assert.Equal(t, "coder-toolgate", gatewayEnv(t, "--set", "config.oidc.audience=coder-toolgate")["TOOLGATE_OIDC_AUDIENCE"])
	})

	t.Run("a list", func(t *testing.T) {
		values := gatewayEnv(t, "--set", "config.oidc.audience={toolgate,coder-toolgate}")
		assert.Equal(t, "toolgate,coder-toolgate", values["TOOLGATE_OIDC_AUDIENCE"])
	})
}

func TestOIDCCLISettings(t *testing.T) {
	env := func(t *testing.T, extraArgs ...string) map[string]string {
		t.Helper()

		manifests, err := render(t, quickStartValues, extraArgs...)
		require.NoError(t, err)

		values := map[string]string{}

		for _, variable := range find[appsv1.Deployment](t, manifests, "Deployment").Spec.Template.Spec.Containers[0].Env {
			values[variable.Name] = variable.Value
		}

		return values
	}

	t.Run("set", func(t *testing.T) {
		values := env(t, "--set", "config.oidc.cliClientID=toolgate-cli", "--set", "config.oidc.cliScopes={openid,email}")
		assert.Equal(t, "toolgate-cli", values["TOOLGATE_OIDC_CLI_CLIENT_ID"])
		assert.Equal(t, "openid,email", values["TOOLGATE_OIDC_CLI_SCOPES"])
	})

	t.Run("unset", func(t *testing.T) {
		values := env(t)
		assert.NotContains(t, values, "TOOLGATE_OIDC_CLI_CLIENT_ID")
		assert.NotContains(t, values, "TOOLGATE_OIDC_CLI_SCOPES")
	})
}

func TestWebUISettings(t *testing.T) {
	gatewayEnv := func(t *testing.T, set ...string) map[string]string {
		t.Helper()

		manifests, err := render(t, append(append([]string{}, quickStartValues...), set...))
		require.NoError(t, err)

		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		values := map[string]string{}

		for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
			values[variable.Name] = variable.Value
		}

		return values
	}

	t.Run("off by default", func(t *testing.T) {
		values := gatewayEnv(t)
		for _, name := range []string{"TOOLGATE_PUBLIC_URL", "TOOLGATE_OIDC_UI_CLIENT_ID", "TOOLGATE_OIDC_UI_CLIENT_SECRET_FILE", "TOOLGATE_OIDC_UI_SCOPES", "TOOLGATE_ADMIN_GROUP"} {
			assert.NotContains(t, values, name)
		}
	})

	t.Run("set from config.ui", func(t *testing.T) {
		values := gatewayEnv(t,
			"config.ui.publicURL=https://toolgate.example.com",
			"config.ui.clientID=toolgate-ui",
			"config.ui.clientSecretFile=/var/run/secrets/toolgate-ui/secret",
			"config.ui.scopes={openid,email,profile,groups}",
			"config.ui.adminGroup=toolgate-admins",
		)
		assert.Equal(t, "https://toolgate.example.com", values["TOOLGATE_PUBLIC_URL"])
		assert.Equal(t, "toolgate-ui", values["TOOLGATE_OIDC_UI_CLIENT_ID"])
		assert.Equal(t, "/var/run/secrets/toolgate-ui/secret", values["TOOLGATE_OIDC_UI_CLIENT_SECRET_FILE"])
		assert.Equal(t, "openid,email,profile,groups", values["TOOLGATE_OIDC_UI_SCOPES"])
		assert.Equal(t, "toolgate-admins", values["TOOLGATE_ADMIN_GROUP"])
	})

	t.Run("client ID needs the public URL", func(t *testing.T) {
		_, err := render(t, append(append([]string{}, quickStartValues...), "config.ui.clientID=toolgate-ui"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "config.ui.publicURL is required")
	})
}

func TestMCPPublicURLSetting(t *testing.T) {
	gatewayEnv := func(t *testing.T, set ...string) map[string]string {
		t.Helper()

		manifests, err := render(t, append(append([]string{}, quickStartValues...), set...))
		require.NoError(t, err)

		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		values := map[string]string{}

		for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
			values[variable.Name] = variable.Value
		}

		return values
	}

	t.Run("unset by default", func(t *testing.T) {
		assert.NotContains(t, gatewayEnv(t), "TOOLGATE_MCP_PUBLIC_URL")
	})

	t.Run("set from config.mcpPublicURL", func(t *testing.T) {
		values := gatewayEnv(t, "config.mcpPublicURL=https://mcp.example.com/mcp")
		assert.Equal(t, "https://mcp.example.com/mcp", values["TOOLGATE_MCP_PUBLIC_URL"])
	})
}

func TestAuditPayloadSettings(t *testing.T) {
	gatewayEnv := func(t *testing.T, set ...string) map[string]corev1.EnvVar {
		t.Helper()

		manifests, err := render(t, append(append([]string{}, quickStartValues...), set...))
		require.NoError(t, err)

		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		values := map[string]corev1.EnvVar{}

		for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
			values[variable.Name] = variable
		}

		return values
	}

	t.Run("payloads are off by default", func(t *testing.T) {
		values := gatewayEnv(t)
		assert.Equal(t, "false", values["TOOLGATE_AUDIT_PAYLOADS"].Value)
		assert.NotContains(t, values, "TOOLGATE_AUDIT_S3_BUCKET")
		assert.NotContains(t, values, "AWS_ACCESS_KEY_ID")
	})

	t.Run("the S3 settings and credentials come from config.audit", func(t *testing.T) {
		values := gatewayEnv(t,
			"config.audit.payloads=true",
			"config.audit.s3.endpoint=https://s3.example.com",
			"config.audit.s3.region=eu-west-1",
			"config.audit.s3.bucket=audit",
			"config.audit.s3.prefix=toolgate/",
			"config.audit.s3.pathStyle=true",
			"config.audit.s3.existingSecret=toolgate-s3",
		)
		assert.Equal(t, "true", values["TOOLGATE_AUDIT_PAYLOADS"].Value)
		assert.Equal(t, "https://s3.example.com", values["TOOLGATE_AUDIT_S3_ENDPOINT"].Value)
		assert.Equal(t, "eu-west-1", values["TOOLGATE_AUDIT_S3_REGION"].Value)
		assert.Equal(t, "audit", values["TOOLGATE_AUDIT_S3_BUCKET"].Value)
		assert.Equal(t, "toolgate/", values["TOOLGATE_AUDIT_S3_PREFIX"].Value)
		assert.Equal(t, "true", values["TOOLGATE_AUDIT_S3_PATH_STYLE"].Value)

		for name, key := range map[string]string{"AWS_ACCESS_KEY_ID": "accessKeyID", "AWS_SECRET_ACCESS_KEY": "secretAccessKey"} {
			require.NotNil(t, values[name].ValueFrom, name)
			require.NotNil(t, values[name].ValueFrom.SecretKeyRef, name)
			assert.Equal(t, "toolgate-s3", values[name].ValueFrom.SecretKeyRef.Name)
			assert.Equal(t, key, values[name].ValueFrom.SecretKeyRef.Key)
			assert.Empty(t, values[name].Value, "the credential is never in the manifest")
		}
	})

	t.Run("payloads on without a bucket is rejected", func(t *testing.T) {
		_, err := render(t, append(append([]string{}, quickStartValues...), "config.audit.payloads=true"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "config.audit.s3.bucket is required when config.audit.payloads is true")
	})
}

func TestResolverSettings(t *testing.T) {
	resolverValues := filepath.Join(t.TempDir(), "resolvers.yaml")
	require.NoError(t, os.WriteFile(resolverValues, []byte(`config:
  resolvers:
    - name: github-app
      url: https://resolver.tools.svc/resolve
      audience: toolgate-resolver-github-app
      timeout: 20s
    - name: vault
      url: https://vault.example/resolve
      audience: toolgate-resolver-vault
`), 0o600))

	t.Run("no resolvers means no configuration file", func(t *testing.T) {
		deployment := find[appsv1.Deployment](t, renderQuickStart(t), "Deployment")
		assert.Equal(t, []string{"serve"}, deployment.Spec.Template.Spec.Containers[0].Args)
		assert.Empty(t, deployment.Spec.Template.Spec.Volumes)
	})

	t.Run("each resolver gets the configuration and its own token", func(t *testing.T) {
		manifests := renderQuickStart(t, "--values", resolverValues)
		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		pod := deployment.Spec.Template.Spec
		container := pod.Containers[0]
		assert.Equal(t, []string{"serve", "--config", "/etc/toolgate/config.yaml"}, container.Args)

		configMap := find[corev1.ConfigMap](t, manifests, "ConfigMap")

		var file struct {
			Resolvers []map[string]string `json:"resolvers"`
		}
		require.NoError(t, yaml.UnmarshalStrict([]byte(configMap.Data["config.yaml"]), &file))
		require.Len(t, file.Resolvers, 2)
		assert.Equal(t, map[string]string{
			"name": "github-app", "url": "https://resolver.tools.svc/resolve",
			"audience": "toolgate-resolver-github-app", "timeout": "20s",
		}, file.Resolvers[0])

		volumes := map[string]corev1.Volume{}
		for _, volume := range pod.Volumes {
			volumes[volume.Name] = volume
		}

		mounts := map[string]corev1.VolumeMount{}
		for _, mount := range container.VolumeMounts {
			mounts[mount.Name] = mount
		}

		require.NotNil(t, volumes["config"].ConfigMap)
		assert.Equal(t, configMap.Name, volumes["config"].ConfigMap.Name)
		assert.Equal(t, "/etc/toolgate/config.yaml", mounts["config"].MountPath)

		for name, audience := range map[string]string{"github-app": "toolgate-resolver-github-app", "vault": "toolgate-resolver-vault"} {
			volume := volumes["resolver-"+name]
			require.NotNil(t, volume.Projected, name)
			require.Len(t, volume.Projected.Sources, 1, name)
			token := volume.Projected.Sources[0].ServiceAccountToken
			require.NotNil(t, token, name)
			assert.Equal(t, audience, token.Audience)
			require.NotNil(t, token.ExpirationSeconds)
			assert.EqualValues(t, 600, *token.ExpirationSeconds)
			assert.Equal(t, "token", token.Path)
			assert.Equal(t, "/var/run/secrets/toolgate/resolvers/"+name, mounts["resolver-"+name].MountPath)
			assert.True(t, mounts["resolver-"+name].ReadOnly)
		}
	})
}

func TestSPIFFETrustDomainSettings(t *testing.T) {
	spiffeValues := filepath.Join(t.TempDir(), "spiffe.yaml")
	require.NoError(t, os.WriteFile(spiffeValues, []byte(`config:
  spiffe:
    trustDomains:
      - name: coder.example.com
        bundleEndpointURL: https://spire.example.com:8443
        caFile: /etc/toolgate/spire-ca.pem
`), 0o600))

	t.Run("trust domains go in the configuration file", func(t *testing.T) {
		manifests := renderQuickStart(t, "--values", spiffeValues)
		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		assert.Equal(t, []string{"serve", "--config", "/etc/toolgate/config.yaml"}, deployment.Spec.Template.Spec.Containers[0].Args)

		configMap := find[corev1.ConfigMap](t, manifests, "ConfigMap")

		var file struct {
			SPIFFETrustDomains []map[string]string `json:"spiffeTrustDomains"`
		}
		require.NoError(t, yaml.UnmarshalStrict([]byte(configMap.Data["config.yaml"]), &file))
		assert.Equal(t, []map[string]string{{
			"name": "coder.example.com", "bundleEndpointURL": "https://spire.example.com:8443", "caFile": "/etc/toolgate/spire-ca.pem",
		}}, file.SPIFFETrustDomains)
	})

	t.Run("a trust domain with both URLs is rejected", func(t *testing.T) {
		_, err := render(t, append(append([]string{}, quickStartValues...),
			"config.spiffe.trustDomains[0].name=coder.example.com",
			"config.spiffe.trustDomains[0].bundleEndpointURL=https://a.example",
			"config.spiffe.trustDomains[0].jwksURL=https://b.example"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "needs exactly one of bundleEndpointURL and jwksURL")
	})
}

func TestWorkspaceAttestationSetting(t *testing.T) {
	values := filepath.Join(t.TempDir(), "attestation.yaml")
	require.NoError(t, os.WriteFile(values, []byte(`config:
  workspaceAttestation:
    namespaces: [coder]
    match: {by: email}
    workspaceID: coder/{username}/{workspace}
`), 0o600))

	manifests := renderQuickStart(t, "--values", values)
	configMap := find[corev1.ConfigMap](t, manifests, "ConfigMap")

	var file struct {
		WorkspaceAttestation map[string]any `json:"workspaceAttestation"`
	}
	require.NoError(t, yaml.UnmarshalStrict([]byte(configMap.Data["config.yaml"]), &file))
	assert.Equal(t, map[string]any{
		"namespaces": []any{"coder"}, "match": map[string]any{"by": "email"}, "workspaceID": "coder/{username}/{workspace}",
	}, file.WorkspaceAttestation)

	_, err := render(t, append(append([]string{}, quickStartValues...), "config.workspaceAttestation.workspaceID=coder/{workspace}"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespaces is required")
}

func TestOIDCAdditionalIssuersSetting(t *testing.T) {
	issuerValues := filepath.Join(t.TempDir(), "issuers.yaml")
	require.NoError(t, os.WriteFile(issuerValues, []byte(`config:
  oidc:
    additionalIssuers:
      - issuerURL: https://idp.example.com/application/o/coder-toolgate/
        audience: coder-toolgate
`), 0o600))

	t.Run("additional issuers go in the configuration file", func(t *testing.T) {
		manifests := renderQuickStart(t, "--values", issuerValues)
		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		assert.Equal(t, []string{"serve", "--config", "/etc/toolgate/config.yaml"}, deployment.Spec.Template.Spec.Containers[0].Args)

		configMap := find[corev1.ConfigMap](t, manifests, "ConfigMap")

		var file struct {
			OIDCAdditionalIssuers []map[string]string `json:"oidcAdditionalIssuers"`
		}
		require.NoError(t, yaml.UnmarshalStrict([]byte(configMap.Data["config.yaml"]), &file))
		assert.Equal(t, []map[string]string{{
			"issuerURL": "https://idp.example.com/application/o/coder-toolgate/", "audience": "coder-toolgate",
		}}, file.OIDCAdditionalIssuers)
	})

	t.Run("without additional issuers there is no configuration file", func(t *testing.T) {
		manifests := renderQuickStart(t)
		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		assert.NotContains(t, deployment.Spec.Template.Spec.Containers[0].Args, "--config")
	})

	t.Run("an issuer without an audience is rejected", func(t *testing.T) {
		_, err := render(t, append(append([]string{}, quickStartValues...),
			"config.oidc.additionalIssuers[0].issuerURL=https://idp.example.com/application/o/coder-toolgate/"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "needs an audience")
	})

	t.Run("an issuer without an issuer URL is rejected", func(t *testing.T) {
		_, err := render(t, append(append([]string{}, quickStartValues...),
			"config.oidc.additionalIssuers[0].audience=coder-toolgate"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "needs an issuerURL")
	})
}

func TestChartTokenExchangesConfigFile(t *testing.T) {
	writeValues := func(t *testing.T, entries string) string {
		t.Helper()

		path := filepath.Join(t.TempDir(), "exchanges.yaml")
		require.NoError(t, os.WriteFile(path, []byte("config:\n  tokenExchanges:\n"+entries), 0o600))

		return path
	}

	t.Run("exchanges go in the configuration file", func(t *testing.T) {
		values := writeValues(t, `    - name: temporal
      tokenURL: https://authentik.example.com/application/o/token/
      clientID: toolgate-token-exchange
      clientSecretFile: /etc/toolgate/exchanges/temporal/client-secret
      audience: temporal-client-id
      timeout: 20s
`)
		manifests := renderQuickStart(t, "--values", values)
		configMap := find[corev1.ConfigMap](t, manifests, "ConfigMap")

		var file struct {
			TokenExchanges []map[string]string `json:"tokenExchanges"`
		}
		require.NoError(t, yaml.UnmarshalStrict([]byte(configMap.Data["config.yaml"]), &file))
		assert.Equal(t, []map[string]string{{
			"name": "temporal", "tokenURL": "https://authentik.example.com/application/o/token/",
			"clientID": "toolgate-token-exchange", "clientSecretFile": "/etc/toolgate/exchanges/temporal/client-secret",
			"audience": "temporal-client-id", "timeout": "20s",
		}}, file.TokenExchanges)

		deployment := find[appsv1.Deployment](t, manifests, "Deployment")
		assert.Equal(t, []string{"serve", "--config", "/etc/toolgate/config.yaml"}, deployment.Spec.Template.Spec.Containers[0].Args)
	})

	failures := map[string]struct{ entry, want string }{
		"missing name":             {"    - {tokenURL: \"https://i.example/t\", clientID: c, clientSecretFile: /s}\n", "name"},
		"bad name":                 {"    - {name: Temporal, tokenURL: \"https://i.example/t\", clientID: c, clientSecretFile: /s}\n", "name \"Temporal\""},
		"missing tokenURL":         {"    - {name: t, clientID: c, clientSecretFile: /s}\n", "needs a tokenURL"},
		"missing clientID":         {"    - {name: t, tokenURL: \"https://i.example/t\", clientSecretFile: /s}\n", "needs a clientID"},
		"missing clientSecretFile": {"    - {name: t, tokenURL: \"https://i.example/t\", clientID: c}\n", "needs a clientSecretFile"},
		"http tokenURL":            {"    - {name: t, tokenURL: \"http://i.example/t\", clientID: c, clientSecretFile: /s}\n", "https://"},
	}

	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			_, err := render(t, quickStartValues, "--values", writeValues(t, failure.entry))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "config.tokenExchanges")
			assert.Contains(t, err.Error(), failure.want)
		})
	}
}
