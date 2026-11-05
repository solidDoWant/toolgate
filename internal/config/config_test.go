package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/config"
)

const validDatabaseURL = "postgres://toolgate@db.example:5432/toolgate"

func envFrom(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func writeFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestLoadDefaultsAndOverrides(t *testing.T) {
	path := writeFile(t, `
listenAddress: ":8443"
databaseURL: postgres://file@db.example/toolgate
shutdownTimeout: 45s
logLevel: debug
`)

	cfg, err := config.Load(path, envFrom(map[string]string{
		"TOOLGATE_DATABASE_URL":               validDatabaseURL,
		"TOOLGATE_OTLP_ENDPOINT":              "http://collector:4318",
		"TOOLGATE_OIDC_ISSUER_URL":            "https://idp.example",
		"TOOLGATE_OIDC_AUDIENCE":              "toolgate",
		"TOOLGATE_OIDC_GROUPS_CLAIM":          "roles",
		"TOOLGATE_OIDC_CLI_CLIENT_ID":         "toolgate-cli",
		"TOOLGATE_SERVICE_ACCOUNT_NAMESPACES": "ci, agents,",
	}))
	require.NoError(t, err)

	assert.Equal(t, config.Config{
		ListenAddress:            ":8443",
		MetricsListenAddress:     ":9090",
		DatabaseURL:              validDatabaseURL,
		ShutdownTimeout:          config.Duration(45 * time.Second),
		LogLevel:                 "debug",
		OTLPEndpoint:             "http://collector:4318",
		OIDCIssuerURL:            "https://idp.example",
		OIDCAudience:             "toolgate",
		OIDCEmailClaim:           "email",
		OIDCEmailVerifiedClaim:   "email_verified",
		OIDCGroupsClaim:          "roles",
		OIDCCLIClientID:          "toolgate-cli",
		OIDCCLIScopes:            []string{"openid", "email", "profile", "offline_access"},
		ServiceAccountNamespaces: []string{"ci", "agents"},
	}, cfg)
}

func TestLoadEnvironmentOnly(t *testing.T) {
	cfg, err := config.Load("", envFrom(map[string]string{
		"TOOLGATE_DATABASE_URL":           validDatabaseURL,
		"TOOLGATE_METRICS_LISTEN_ADDRESS": "127.0.0.1:9100",
		"TOOLGATE_SHUTDOWN_TIMEOUT":       "5s",
		"TOOLGATE_LOG_LEVEL":              "warn",
		"TOOLGATE_OIDC_ISSUER_URL":        "https://idp.example",
		"TOOLGATE_OIDC_AUDIENCE":          "toolgate",
	}))
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1:9100", cfg.MetricsListenAddress)
	assert.Equal(t, config.Duration(5*time.Second), cfg.ShutdownTimeout)
	assert.Equal(t, "warn", cfg.LogLevel)
	assert.Equal(t, ":8080", cfg.ListenAddress)
}

// Every problem in the configuration is reported, each naming its key.
func TestLoadReportsEveryProblem(t *testing.T) {
	tests := []struct {
		name         string
		file         string
		env          map[string]string
		wantProblems []string
		wantAbsent   []string
	}{
		{
			name:         "missing database URL",
			env:          map[string]string{},
			wantProblems: []string{"databaseURL: required", "oidcIssuerURL: required", "oidcAudience: required"},
		},
		{
			name: "invalid OIDC values",
			file: `
oidcIssuerURL: "idp.example"
oidcEmailClaim: ""
oidcGroupsClaim: ""
oidcCLIScopes: []
`,
			wantProblems: []string{
				`oidcIssuerURL: "idp.example" is not an http or https URL`,
				"oidcAudience: required",
				"oidcEmailClaim: must not be empty",
				"oidcGroupsClaim: must not be empty",
				"oidcCLIScopes: must not be empty",
			},
			wantAbsent: []string{"oidcEmailVerifiedClaim"},
		},
		{
			name: "invalid service-account namespace",
			file: `
serviceAccountNamespaces: [ci, "Not_A_Namespace"]
`,
			wantProblems: []string{`serviceAccountNamespaces: "Not_A_Namespace" is not a namespace name`},
			wantAbsent:   []string{`"ci"`},
		},
		{
			name: "several invalid environment values",
			env: map[string]string{
				"TOOLGATE_LISTEN_ADDRESS":   "no-port",
				"TOOLGATE_SHUTDOWN_TIMEOUT": "soon",
				"TOOLGATE_LOG_LEVEL":        "loud",
				"TOOLGATE_OTLP_ENDPOINT":    "collector:4318",
			},
			wantProblems: []string{
				"databaseURL: required",
				`listenAddress: "no-port" is not a host:port address`,
				"TOOLGATE_SHUTDOWN_TIMEOUT: must be a duration",
				`logLevel: "loud" is not one of`,
				`otlpEndpoint: "collector:4318" is not an http or https URL`,
			},
		},
		{
			name: "invalid file values",
			file: `
metricsListenAddress: ":8080"
databaseURL: "postgres://user:hunter2@db.example:notaport/x"
shutdownTimeout: 0s
`,
			wantProblems: []string{
				"metricsListenAddress: must differ from listenAddress",
				"databaseURL: not a valid PostgreSQL connection string",
				"shutdownTimeout: must be greater than zero",
			},
			wantAbsent: []string{"hunter2"},
		},
		{
			name:         "unknown key in file",
			file:         "listenAdress: \":80\"\n",
			wantProblems: []string{`unknown field "listenAdress"`},
		},
		{
			name:         "missing file",
			file:         "-",
			wantProblems: []string{"configuration file"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := ""

			switch test.file {
			case "":
			case "-":
				path = filepath.Join(t.TempDir(), "absent.yaml")
			default:
				path = writeFile(t, test.file)
			}

			_, err := config.Load(path, envFrom(test.env))
			require.Error(t, err)

			for _, problem := range test.wantProblems {
				assert.Contains(t, err.Error(), problem)
			}

			for _, absent := range test.wantAbsent {
				assert.NotContains(t, err.Error(), absent)
			}
		})
	}
}
