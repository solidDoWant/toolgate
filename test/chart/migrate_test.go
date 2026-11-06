package chart

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

var splitValues = []string{
	"config.database.migrateOnStart=false",
	"config.database.migrationSecret=toolgate-migrator",
	"config.database.migrationEgress[0].ports[0].port=5432",
}

func hookOf(t *testing.T, object manifest) (string, string) {
	t.Helper()

	var parsed struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}

	require.NoError(t, yaml.Unmarshal(object.raw, &parsed))
	assert.Equal(t, "pre-install,pre-upgrade", parsed.Metadata.Annotations["helm.sh/hook"], object.Kind)
	assert.Equal(t, "before-hook-creation,hook-succeeded", parsed.Metadata.Annotations["helm.sh/hook-delete-policy"], object.Kind)

	return parsed.Metadata.Annotations["helm.sh/hook-weight"], object.Metadata.Name
}

func TestDefaultValuesRenderNoMigrationHook(t *testing.T) {
	for _, set := range [][]string{
		nil,
		// A migration Secret alone changes nothing: the gateway migrates.
		{"config.database.migrationSecret=toolgate-migrator"},
		// Without a Secret, nothing runs the migrations.
		{"config.database.migrateOnStart=false"},
	} {
		manifests := renderWith(t, set...)
		assert.Empty(t, ofKind(manifests, "Job"), "%v", set)
		assert.Empty(t, ofKind(manifests, "NetworkPolicy"), "%v", set)

		for _, candidate := range manifests {
			assert.NotContains(t, string(candidate.raw), "helm.sh/hook:", "%v: %s", set, candidate.Kind)
		}
	}

	deployment := find[appsv1.Deployment](t, renderWith(t), "Deployment")

	var onStart string

	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		if variable.Name == "TOOLGATE_MIGRATE_ON_START" {
			onStart = variable.Value
		}
	}

	assert.Equal(t, "true", onStart)
}

func TestMigrationHookObjects(t *testing.T) {
	manifests := renderWith(t, splitValues...)

	weightOf := func(kind string) string {
		require.Len(t, ofKind(manifests, kind), 1, kind)

		weight, name := hookOf(t, ofKind(manifests, kind)[0])
		if kind != "Job" {
			assert.Equal(t, "toolgate-migrate", name)
		}

		return weight
	}

	// The weights order the objects: the ServiceAccount, the policy, the Job.
	accountWeight, policyWeight, jobWeight := "", weightOf("NetworkPolicy"), weightOf("Job")

	for _, candidate := range ofKind(manifests, "ServiceAccount") {
		if candidate.Metadata.Name == "toolgate-migrate" {
			accountWeight, _ = hookOf(t, candidate)
		}
	}

	assert.Equal(t, "-20", accountWeight)
	assert.Equal(t, "-10", policyWeight)
	assert.Equal(t, "0", jobWeight)

	job := find[batchv1.Job](t, manifests, "Job")
	assert.Equal(t, "toolgate-migrate", job.Name)
	require.NotNil(t, job.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(240), *job.Spec.ActiveDeadlineSeconds)
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, int32(2), *job.Spec.BackoffLimit)

	spec := job.Spec.Template.Spec
	assert.Equal(t, corev1.RestartPolicyNever, spec.RestartPolicy)
	assert.Equal(t, "toolgate-migrate", spec.ServiceAccountName)
	require.NotNil(t, spec.AutomountServiceAccountToken)
	assert.False(t, *spec.AutomountServiceAccountToken)
	require.NotNil(t, spec.SecurityContext)
	assert.Equal(t, int64(65532), *spec.SecurityContext.RunAsUser)
	require.Len(t, spec.Containers, 1)
	assert.Equal(t, []string{"migrate"}, spec.Containers[0].Args)
	require.NotNil(t, spec.Containers[0].SecurityContext)
	assert.False(t, *spec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
	assert.True(t, *spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem)

	require.Len(t, spec.Containers[0].Env, 1)
	assert.Equal(t, "TOOLGATE_DATABASE_URL", spec.Containers[0].Env[0].Name)
	assert.Equal(t, "toolgate-migrator", spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "url", spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Key)

	// The gateway gets the app role's Secret, and does not migrate.
	deployment := find[appsv1.Deployment](t, manifests, "Deployment")
	env := map[string]corev1.EnvVar{}

	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		env[variable.Name] = variable
	}

	assert.Equal(t, "false", env["TOOLGATE_MIGRATE_ON_START"].Value)
	assert.Equal(t, "toolgate-database", env["TOOLGATE_DATABASE_URL"].ValueFrom.SecretKeyRef.Name)

	policy := find[networkingv1.NetworkPolicy](t, manifests, "NetworkPolicy")
	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
	require.NoError(t, err)
	assert.True(t, selector.Matches(labels.Set(job.Spec.Template.Labels)), "the policy selects the Job's pods")

	deploymentLabels := labels.Set(deployment.Spec.Template.Labels)
	assert.False(t, selector.Matches(deploymentLabels), "the policy leaves the gateway pods alone")
	assert.Empty(t, policy.Spec.Ingress)
	assert.ElementsMatch(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, policy.Spec.PolicyTypes)
	require.Len(t, policy.Spec.Egress, 2)
	require.Len(t, policy.Spec.Egress[0].Ports, 2)
	assert.Equal(t, 53, policy.Spec.Egress[0].Ports[0].Port.IntValue())
	assert.Equal(t, 53, policy.Spec.Egress[0].Ports[1].Port.IntValue())
	assert.Equal(t, 5432, policy.Spec.Egress[1].Ports[0].Port.IntValue())
}

func TestMigrationJobRequiresEgress(t *testing.T) {
	_, err := render(t, append([]string{
		"config.database.migrateOnStart=false",
		"config.database.migrationSecret=toolgate-migrator",
	}, quickStartValues...))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "migrationEgress")
}

func TestMigrationJobMountsNothingFromTheRelease(t *testing.T) {
	manifests := renderWith(t, append([]string{
		`config.resolvers[0].name=billing`,
		`config.resolvers[0].url=https://billing.example.com`,
		`config.resolvers[0].audience=billing`,
		`config.resolvers[0].caFile=/etc/ca/ca.pem`,
		`config.resolvers[1].name=other`,
		`config.resolvers[1].url=https://other.example.com`,
		`config.resolvers[1].audience=other`,
		`resources.persistence.ca.type=secret`,
		`resources.persistence.ca.name=resolver-ca`,
		`resources.persistence.ca.advancedMounts.gateway.gateway[0].path=/etc/ca`,
		`resources.persistence.client.type=secret`,
		`resources.persistence.client.name=pg-client`,
		`resources.persistence.client.advancedMounts.migrate.migrate[0].path=/etc/pg`,
	}, splitValues...)...)

	job := find[batchv1.Job](t, manifests, "Job")
	require.Len(t, job.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "pg-client", job.Spec.Template.Spec.Volumes[0].Secret.SecretName)
	require.Len(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, 1)
	assert.Equal(t, "/etc/pg", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath)

	deployment := find[appsv1.Deployment](t, manifests, "Deployment")
	paths := map[string]bool{}

	for _, mount := range deployment.Spec.Template.Spec.Containers[0].VolumeMounts {
		paths[mount.MountPath] = true
	}

	assert.Contains(t, paths, "/etc/toolgate/config.yaml")
	assert.Contains(t, paths, "/var/run/secrets/toolgate/resolvers/billing")
	assert.Contains(t, paths, "/var/run/secrets/toolgate/resolvers/other")
	assert.Contains(t, paths, "/etc/ca")
	assert.NotContains(t, paths, "/etc/pg")
}
