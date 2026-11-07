package chart

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	inlineGatewayURL   = "postgres://toolgate_app@db/toolgate?sslmode=verify-full"
	inlineMigrationURL = "postgres://toolgate_migrator@db/toolgate?sslmode=verify-full"
)

// withoutDatabaseSecret is the quick-start values minus the gateway's Secret.
func withoutDatabaseSecret() []string {
	var set []string

	for _, value := range quickStartValues {
		if !strings.HasPrefix(value, "config.database.existingSecret=") {
			set = append(set, value)
		}
	}

	return set
}

func databaseURLVariable(t *testing.T, variables []corev1.EnvVar) corev1.EnvVar {
	t.Helper()

	var found []corev1.EnvVar

	for _, variable := range variables {
		if variable.Name == "TOOLGATE_DATABASE_URL" {
			found = append(found, variable)
		}
	}

	require.Len(t, found, 1)

	return found[0]
}

func TestGatewayDatabaseURLSource(t *testing.T) {
	tests := map[string]struct {
		set       []string
		wantValue string
		wantKey   string
		wantErr   string
	}{
		"secret alone": {
			set:     append(withoutDatabaseSecret(), "config.database.existingSecret=db-secret"),
			wantKey: "db-secret",
		},
		"inline alone": {
			set:       append(withoutDatabaseSecret(), "config.database.url="+inlineGatewayURL),
			wantValue: inlineGatewayURL,
		},
		"neither": {
			set:     withoutDatabaseSecret(),
			wantErr: "config.database.url or config.database.existingSecret is required",
		},
		"both": {
			set:     append(withoutDatabaseSecret(), "config.database.existingSecret=db-secret", "config.database.url="+inlineGatewayURL),
			wantErr: "config.database.url and config.database.existingSecret are both set",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			manifests, err := render(t, test.set)
			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)

				return
			}

			require.NoError(t, err)

			deployment := find[appsv1.Deployment](t, manifests, "Deployment")
			variable := databaseURLVariable(t, deployment.Spec.Template.Spec.Containers[0].Env)

			assert.Equal(t, test.wantValue, variable.Value)

			if test.wantKey == "" {
				assert.Nil(t, variable.ValueFrom)

				return
			}

			require.NotNil(t, variable.ValueFrom)
			assert.Equal(t, test.wantKey, variable.ValueFrom.SecretKeyRef.Name)
			assert.Equal(t, "url", variable.ValueFrom.SecretKeyRef.Key)
		})
	}
}

func TestMigrationJobDatabaseURLSource(t *testing.T) {
	base := []string{"config.database.migrateOnStart=false"}

	tests := map[string]struct {
		set       []string
		wantValue string
		wantKey   string
		wantErr   string
	}{
		"secret alone": {
			set:     []string{"config.database.migrationSecret=migrator"},
			wantKey: "migrator",
		},
		"inline alone": {
			set:       []string{"config.database.migrationUrl=" + inlineMigrationURL},
			wantValue: inlineMigrationURL,
		},
		"inline migration with secret gateway": {
			set:       []string{"config.database.migrationUrl=" + inlineMigrationURL, "config.database.existingSecret=db-secret"},
			wantValue: inlineMigrationURL,
		},
		"secret migration with inline gateway": {
			set:     []string{"config.database.migrationSecret=migrator", "config.database.url=" + inlineGatewayURL},
			wantKey: "migrator",
		},
		"both": {
			set:     []string{"config.database.migrationSecret=migrator", "config.database.migrationUrl=" + inlineMigrationURL},
			wantErr: "config.database.migrationUrl and config.database.migrationSecret are both set",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			set := append(withoutDatabaseSecret(), base...)
			if !strings.Contains(name, "inline gateway") {
				set = append(set, "config.database.existingSecret=toolgate-database")
			}

			// Later values win, so the case's own values come last.
			manifests, err := render(t, append(set, test.set...))
			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)

				return
			}

			require.NoError(t, err)

			job := find[batchv1.Job](t, manifests, "Job")
			variable := databaseURLVariable(t, job.Spec.Template.Spec.Containers[0].Env)

			assert.Equal(t, test.wantValue, variable.Value)

			if test.wantKey == "" {
				assert.Nil(t, variable.ValueFrom)

				return
			}

			require.NotNil(t, variable.ValueFrom)
			assert.Equal(t, test.wantKey, variable.ValueFrom.SecretKeyRef.Name)
			assert.Equal(t, "url", variable.ValueFrom.SecretKeyRef.Key)
		})
	}
}
