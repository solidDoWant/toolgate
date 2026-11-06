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
		"TOOLGATE_PUBLIC_URL":                 "https://toolgate.example",
		"TOOLGATE_OIDC_UI_CLIENT_ID":          "toolgate-ui",
		"TOOLGATE_ADMIN_GROUP":                "toolgate-admins",
	}))
	require.NoError(t, err)

	assert.Equal(t, config.Config{
		ListenAddress:            ":8443",
		MetricsListenAddress:     ":9090",
		DatabaseURL:              validDatabaseURL,
		MigrateOnStart:           true,
		ShutdownTimeout:          config.Duration(45 * time.Second),
		LogLevel:                 "debug",
		OTLPEndpoint:             "http://collector:4318",
		OIDCIssuerURL:            "https://idp.example",
		OIDCAudience:             config.Audiences{"toolgate"},
		OIDCEmailClaim:           "email",
		OIDCEmailVerifiedClaim:   "email_verified",
		OIDCGroupsClaim:          "roles",
		OIDCCLIClientID:          "toolgate-cli",
		OIDCCLIScopes:            []string{"openid", "email", "profile", "offline_access"},
		ServiceAccountNamespaces: []string{"ci", "agents"},
		InstanceLimitPerUser:     20,
		InstanceLimit:            200,
		SessionIdleTimeout:       config.Duration(2 * time.Hour),
		ApprovalTimeout:          config.Duration(10 * time.Minute),
		PublicURL:                "https://toolgate.example",
		OIDCUIClientID:           "toolgate-ui",
		OIDCUIScopes:             []string{"openid", "email", "profile"},
		AdminGroup:               "toolgate-admins",
	}, cfg)
}

func TestLoadEnvironmentOnly(t *testing.T) {
	cfg, err := config.Load("", envFrom(map[string]string{
		"TOOLGATE_DATABASE_URL":            validDatabaseURL,
		"TOOLGATE_METRICS_LISTEN_ADDRESS":  "127.0.0.1:9100",
		"TOOLGATE_SHUTDOWN_TIMEOUT":        "5s",
		"TOOLGATE_LOG_LEVEL":               "warn",
		"TOOLGATE_OIDC_ISSUER_URL":         "https://idp.example",
		"TOOLGATE_OIDC_AUDIENCE":           "toolgate",
		"TOOLGATE_INSTANCE_LIMIT_PER_USER": "1",
		"TOOLGATE_INSTANCE_LIMIT":          "1",
		"TOOLGATE_SHARED_NAMESPACE":        "toolgate-shared",
	}))
	require.NoError(t, err)

	assert.Equal(t, 1, cfg.InstanceLimitPerUser, "1 is the smallest instance limit")
	assert.Equal(t, 1, cfg.InstanceLimit)
	assert.Equal(t, "toolgate-shared", cfg.SharedNamespace)
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
			name: "web UI without public URL",
			file: `
oidcUIClientID: toolgate-ui
`,
			wantProblems: []string{"publicURL: required with oidcUIClientID"},
		},
		{
			name: "web UI with plain http public URL",
			file: `
oidcUIClientID: toolgate-ui
publicURL: "http://toolgate.example.com"
`,
			wantProblems: []string{`publicURL: "http://toolgate.example.com" must be https://`},
		},
		{
			name: "public URL with a path",
			file: `
publicURL: "https://toolgate.example.com/gateway"
`,
			wantProblems: []string{`publicURL: "https://toolgate.example.com/gateway" is not a URL with only a scheme, host and port`},
		},
		{
			name: "web UI with https public URL",
			file: `
oidcUIClientID: toolgate-ui
publicURL: "https://toolgate.example.com"
`,
			wantAbsent: []string{"publicURL"},
		},
		{
			name: "web UI on localhost",
			file: `
oidcUIClientID: toolgate-ui
publicURL: "http://localhost:8080"
`,
			wantAbsent: []string{"publicURL"},
		},
		{
			name: "web UI on 127.0.0.1",
			file: `
oidcUIClientID: toolgate-ui
publicURL: "http://127.0.0.1:8080"
`,
			wantAbsent: []string{"publicURL"},
		},
		{
			name:       "web UI off without public URL",
			file:       "adminGroup: admins\n",
			wantAbsent: []string{"publicURL"},
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
			name: "invalid lease namespace",
			file: `
leaseNamespace: "Not_A_Namespace"
`,
			wantProblems: []string{`leaseNamespace: "Not_A_Namespace" is not a namespace name`},
		},
		{
			name: "lease namespace without gateway pod labels",
			file: `
leaseNamespace: toolgate
`,
			wantProblems: []string{"gatewayPodLabels: required with leaseNamespace"},
		},
		{
			name: "zero session idle timeout",
			file: `
sessionIdleTimeout: 0s
`,
			wantProblems: []string{"sessionIdleTimeout: must be greater than zero"},
		},
		{
			name: "approval timeout under 1 minute",
			file: `
approvalTimeout: 59s
`,
			wantProblems: []string{"approvalTimeout: must be from 1m to 1h"},
		},
		{
			name: "approval timeout over 1 hour",
			file: `
approvalTimeout: 1h0m1s
`,
			wantProblems: []string{"approvalTimeout: must be from 1m to 1h"},
		},
		{
			name: "invalid shared namespace",
			file: `
sharedNamespace: "Not_A_Namespace"
`,
			wantProblems: []string{`sharedNamespace: "Not_A_Namespace" is not a namespace name`},
		},
		{
			name: "shared namespace with the instance namespace prefix",
			file: `
sharedNamespace: tg-shared
`,
			wantProblems: []string{`sharedNamespace: "tg-shared" must not start with tg-`},
		},
		{
			name: "invalid catalog namespace and pod labels",
			file: `
catalogNamespaces: [catalog, "Not_A_Namespace"]
gatewayPodLabels: [app.kubernetes.io/name=toolgate, "no-value", "bad key=x"]
`,
			wantProblems: []string{
				`catalogNamespaces: "Not_A_Namespace" is not a namespace name`,
				`gatewayPodLabels: "no-value" is not a key=value label`,
				`gatewayPodLabels: "bad key=x" is not a key=value label`,
			},
			wantAbsent: []string{`"catalog"`, `"app.kubernetes.io/name=toolgate"`},
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
			name: "instance limits below 1",
			file: `
instanceLimitPerUser: 0
instanceLimit: -1
`,
			wantProblems: []string{"instanceLimitPerUser: must be at least 1", "instanceLimit: must be at least 1"},
		},
		{
			name:         "instance limit that isn't a number",
			env:          map[string]string{"TOOLGATE_INSTANCE_LIMIT": "many"},
			wantProblems: []string{`TOOLGATE_INSTANCE_LIMIT: "many" is not a whole number`},
		},
		{
			name:         "unknown key in file",
			file:         "listenAdress: \":80\"\n",
			wantProblems: []string{`unknown field "listenAdress"`},
		},
		{
			name:         "audit payloads without a bucket",
			file:         "auditPayloads: true\n",
			wantProblems: []string{"auditS3Bucket: required when auditPayloads is true"},
		},
		{
			name:         "audit payloads with a bucket",
			file:         "auditPayloads: true\nauditS3Bucket: audit\n",
			wantAbsent:   []string{"auditS3Bucket"},
			wantProblems: []string{"databaseURL: required"},
		},
		{
			name:         "invalid audit S3 endpoint",
			file:         "auditS3Endpoint: \"s3.example\"\n",
			wantProblems: []string{`auditS3Endpoint: "s3.example" is not an http or https URL`},
		},
		{
			name:         "invalid audit payloads flag",
			env:          map[string]string{"TOOLGATE_AUDIT_PAYLOADS": "maybe"},
			wantProblems: []string{`TOOLGATE_AUDIT_PAYLOADS: "maybe" is not true or false`},
		},
		{
			name:         "OTLP ingest without an endpoint",
			file:         "otlpIngest: true\n",
			wantProblems: []string{"otlpEndpoint: required when otlpIngest is true"},
		},
		{
			name:         "OTLP ingest with an endpoint",
			file:         "otlpIngest: true\notlpEndpoint: http://collector:4318\n",
			wantAbsent:   []string{"otlpEndpoint"},
			wantProblems: []string{"databaseURL: required"},
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

func TestLoadAuditPayloadSettings(t *testing.T) {
	assert.False(t, config.Default().AuditPayloads, "payloads are off by default")

	cfg, err := config.Load("", envFrom(map[string]string{
		"TOOLGATE_DATABASE_URL":        validDatabaseURL,
		"TOOLGATE_OIDC_ISSUER_URL":     "https://idp.example",
		"TOOLGATE_OIDC_AUDIENCE":       "toolgate",
		"TOOLGATE_AUDIT_PAYLOADS":      "true",
		"TOOLGATE_AUDIT_S3_ENDPOINT":   "http://s3.example:9000",
		"TOOLGATE_AUDIT_S3_REGION":     "eu-west-1",
		"TOOLGATE_AUDIT_S3_BUCKET":     "audit",
		"TOOLGATE_AUDIT_S3_PREFIX":     "toolgate/",
		"TOOLGATE_AUDIT_S3_PATH_STYLE": "true",
	}))
	require.NoError(t, err)

	assert.True(t, cfg.AuditPayloads)
	assert.Equal(t, "http://s3.example:9000", cfg.AuditS3Endpoint)
	assert.Equal(t, "eu-west-1", cfg.AuditS3Region)
	assert.Equal(t, "audit", cfg.AuditS3Bucket)
	assert.Equal(t, "toolgate/", cfg.AuditS3Prefix)
	assert.True(t, cfg.AuditS3PathStyle)
}

func TestLoadTelemetrySettings(t *testing.T) {
	assert.False(t, config.Default().OTLPIngest, "ingest is off by default")
	assert.False(t, config.Default().TelemetryAllowContent, "content is removed by default")

	path := writeFile(t, "otlpIngest: true\notlpEndpoint: http://collector:4318\ntelemetry.allowContent: true\n")
	cfg, err := config.Load(path, envFrom(map[string]string{
		"TOOLGATE_DATABASE_URL":    validDatabaseURL,
		"TOOLGATE_OIDC_ISSUER_URL": "https://idp.example",
		"TOOLGATE_OIDC_AUDIENCE":   "toolgate",
	}))
	require.NoError(t, err)

	assert.True(t, cfg.OTLPIngest)
	assert.True(t, cfg.TelemetryAllowContent)
}

func TestWatchedNamespacesAndPodLabels(t *testing.T) {
	cfg := config.Config{LeaseNamespace: "toolgate", GatewayPodLabels: []string{"app=toolgate", "tier=gateway"}}

	assert.Equal(t, []string{"toolgate"}, cfg.WatchedNamespaces(), "the lease namespace is the default")
	assert.Equal(t, map[string]string{"app": "toolgate", "tier": "gateway"}, cfg.PodLabels())

	cfg.CatalogNamespaces = []string{"catalog-a", "catalog-b"}
	assert.Equal(t, []string{"catalog-a", "catalog-b"}, cfg.WatchedNamespaces())

	assert.Empty(t, config.Config{}.WatchedNamespaces(), "no catalog without Kubernetes")
}

func TestApprovalTimeoutBounds(t *testing.T) {
	for _, value := range []string{"1m", "1h"} {
		t.Run(value, func(t *testing.T) {
			cfg, err := config.Load("", envFrom(map[string]string{
				"TOOLGATE_DATABASE_URL":     validDatabaseURL,
				"TOOLGATE_APPROVAL_TIMEOUT": value,
				"TOOLGATE_OIDC_ISSUER_URL":  "https://idp.example",
				"TOOLGATE_OIDC_AUDIENCE":    "toolgate",
			}))
			require.NoError(t, err)

			parsed, err := time.ParseDuration(value)
			require.NoError(t, err)
			assert.Equal(t, config.Duration(parsed), cfg.ApprovalTimeout)
		})
	}
}

func TestLoadDatabaseURLReadsOnlyTheDatabaseURL(t *testing.T) {
	path := writeFile(t, "databaseURL: "+validDatabaseURL+"\nlistenAddress: nonsense\nunknownKey: 1\n")

	got, err := config.LoadDatabaseURL(path, func(string) string { return "" })
	require.NoError(t, err)
	assert.Equal(t, validDatabaseURL, got)

	got, err = config.LoadDatabaseURL("", func(name string) string {
		if name == "TOOLGATE_DATABASE_URL" {
			return "postgres://other@db:5432/x"
		}

		return ""
	})
	require.NoError(t, err)
	assert.Equal(t, "postgres://other@db:5432/x", got)

	_, err = config.LoadDatabaseURL("", func(string) string { return "" })
	require.Error(t, err)
}

func TestMigrateOnStartDefaultsToTrue(t *testing.T) {
	assert.True(t, config.Default().MigrateOnStart)
}
