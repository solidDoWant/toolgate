package chart

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	rbacv1 "k8s.io/api/rbac/v1"
)

var persistentVolumeRules = []rbacv1.PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"persistentvolumes"}, Verbs: []string{"get", "patch", "delete"}},
	{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"}, Verbs: []string{"get"}},
}

func gatewayClusterRoleRules(t *testing.T, manifests []manifest) []rbacv1.PolicyRule {
	t.Helper()

	return find[rbacv1.ClusterRole](t, manifests, "ClusterRole").Rules
}

func mentionsVolumes(rules []rbacv1.PolicyRule) bool {
	for _, rule := range rules {
		for _, resource := range rule.Resources {
			if resource == "persistentvolumes" || resource == "storageclasses" {
				return true
			}
		}
	}

	return false
}

func TestPersistentVolumesAreOffByDefault(t *testing.T) {
	manifests := renderQuickStart(t)

	assert.False(t, mentionsVolumes(gatewayClusterRoleRules(t, manifests)), "no PersistentVolume or StorageClass rule")

	text := admissionPolicyText(t, manifests)
	assert.NotContains(t, text, "isVolume")
	assert.NotContains(t, text, "isOwnVolume")

	assert.Equal(t, "false", gatewayEnv(t, manifests)["TOOLGATE_PERSISTENT_VOLUMES_ENABLED"])
}

func TestPersistentVolumesEnabled(t *testing.T) {
	manifests := renderQuickStart(t, "--set", "config.persistentVolumes.enabled=true",
		"--set", "config.persistentVolumes.unusedRetention=48h", "--set", "config.persistentVolumes.limitPerUser=5Gi")

	t.Run("the ClusterRole holds exactly the volume rules", func(t *testing.T) {
		rules := gatewayClusterRoleRules(t, manifests)
		for _, want := range persistentVolumeRules {
			assert.Contains(t, rules, want)
		}

		var volumeRules []rbacv1.PolicyRule

		for _, rule := range rules {
			if mentionsVolumes([]rbacv1.PolicyRule{rule}) {
				volumeRules = append(volumeRules, rule)
			}
		}

		assert.Equal(t, persistentVolumeRules, volumeRules)
	})

	t.Run("the settings reach the gateway", func(t *testing.T) {
		env := gatewayEnv(t, manifests)
		assert.Equal(t, "true", env["TOOLGATE_PERSISTENT_VOLUMES_ENABLED"])
		assert.Equal(t, "48h", env["TOOLGATE_PERSISTENT_VOLUMES_UNUSED_RETENTION"])
		assert.Equal(t, "5Gi", env["TOOLGATE_PERSISTENT_VOLUMES_LIMIT_PER_USER"])
	})

	t.Run("the admission policy guards volumes", func(t *testing.T) {
		text := admissionPolicyText(t, manifests)
		assert.Contains(t, text, "isVolume:")
		assert.Contains(t, text, "isOwnVolume:")
		assert.Equal(t, 2, strings.Count(text, "|| variables.isOwnVolume"), "both namespace checks allow it")
		assert.Contains(t, text, "persistentVolumeReclaimPolicy")
	})
}

func TestPersistentVolumeDefaults(t *testing.T) {
	env := gatewayEnv(t, renderQuickStart(t, "--set", "config.persistentVolumes.enabled=true"))
	assert.Equal(t, "720h", env["TOOLGATE_PERSISTENT_VOLUMES_UNUSED_RETENTION"])
	assert.Equal(t, "10Gi", env["TOOLGATE_PERSISTENT_VOLUMES_LIMIT_PER_USER"])
}
