// Package config loads and validates the gateway's configuration from an
// optional YAML file and from environment variables.
//
// The doc comments on the Config fields are the configuration reference:
// `make generate-docs` turns them into docs/admin/configuration.md.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// FileEnv is the environment variable naming the configuration file. The
// `--config` flag of `toolgate serve` takes precedence over it.
const FileEnv = "TOOLGATE_CONFIG"

// maxFileSize bounds the configuration file the gateway reads.
const maxFileSize = 1 << 20

// Config is the gateway's configuration. Each field is set from the YAML key
// in its `json` tag, then overridden by the environment variable in its `env`
// tag when that variable is set and not empty.
type Config struct {
	// The address where the gateway listens for MCP and API requests and for
	// the `/healthz` and `/readyz` probes.
	ListenAddress string `json:"listenAddress" env:"TOOLGATE_LISTEN_ADDRESS"`

	// The address of the separate listener that serves Prometheus metrics at
	// `/metrics`. It must differ from `listenAddress`.
	MetricsListenAddress string `json:"metricsListenAddress" env:"TOOLGATE_METRICS_LISTEN_ADDRESS"`

	// The PostgreSQL connection string, as a URL or as key/value pairs.
	// Required. When it starts, the gateway creates and updates its tables in
	// this database. If it cannot, it stops with an error. `/readyz` answers
	// 503 while the gateway cannot connect to this database.
	DatabaseURL string `json:"databaseURL" env:"TOOLGATE_DATABASE_URL"`

	// After SIGTERM, the gateway stops accepting requests and waits this long
	// for open requests to finish. Then it stops. Write it as a Go duration,
	// such as `30s`.
	ShutdownTimeout Duration `json:"shutdownTimeout" env:"TOOLGATE_SHUTDOWN_TIMEOUT"`

	// The lowest level of the JSON log lines that the gateway writes to
	// stdout: `debug`, `info`, `warn` or `error`.
	LogLevel string `json:"logLevel" env:"TOOLGATE_LOG_LEVEL"`

	// The OTLP/HTTP endpoint that receives the traces of the gateway, such as
	// `http://otel-collector:4318`. The gateway sends traces to `/v1/traces`
	// under this URL. If it is empty, the gateway does not export traces.
	OTLPEndpoint string `json:"otlpEndpoint" env:"TOOLGATE_OTLP_ENDPOINT"`

	// The issuer URL of the OIDC identity provider (IdP) that users sign in
	// with, such as `https://idp.example.com/realms/main`. Required. The
	// gateway reads the IdP's discovery document and signing keys from it,
	// and accepts only access tokens this issuer signed.
	OIDCIssuerURL string `json:"oidcIssuerURL" env:"TOOLGATE_OIDC_ISSUER_URL"`

	// The audience that users' access tokens must contain, such as
	// `toolgate`. Required. The gateway refuses a token for another audience.
	OIDCAudience string `json:"oidcAudience" env:"TOOLGATE_OIDC_AUDIENCE"`

	// The access token claim that holds the user's email address.
	OIDCEmailClaim string `json:"oidcEmailClaim" env:"TOOLGATE_OIDC_EMAIL_CLAIM"`

	// The access token claim that tells whether the IdP verified the email
	// address. The gateway uses the email only when this claim is `true`.
	OIDCEmailVerifiedClaim string `json:"oidcEmailVerifiedClaim" env:"TOOLGATE_OIDC_EMAIL_VERIFIED_CLAIM"`

	// The access token claim that holds the user's groups, as a list of
	// strings. The gateway refuses a token if this claim has another type.
	OIDCGroupsClaim string `json:"oidcGroupsClaim" env:"TOOLGATE_OIDC_GROUPS_CLAIM"`

	// The client ID that `toolgate login` signs in with, such as
	// `toolgate-cli`. It must be a public client (one without a secret) with
	// the OAuth 2.0 device authorization grant. The gateway publishes it, with
	// `oidcIssuerURL`, `oidcCLIScopes` and `oidcEmailClaim`, at
	// `/.well-known/toolgate-client` without authentication. If it is empty,
	// users must pass `--client-id` to `toolgate login`.
	OIDCCLIClientID string `json:"oidcCLIClientID" env:"TOOLGATE_OIDC_CLI_CLIENT_ID"`

	// The scopes that `toolgate login` asks for. In the environment variable,
	// separate the scopes with commas. Keep `offline_access`: without it, the
	// IdP gives no refresh token and users must sign in again when their
	// access token expires.
	OIDCCLIScopes []string `json:"oidcCLIScopes" env:"TOOLGATE_OIDC_CLI_SCOPES"`

	// The namespaces whose pods may sign in with a Kubernetes service-account
	// token, such as `[ci, agents]`. In the environment variable, separate
	// the names with commas. A pod signs in with a projected token for the
	// audience `toolgate`, and its workspace is the pod's owner. If the list
	// is empty, only users can sign in.
	ServiceAccountNamespaces []string `json:"serviceAccountNamespaces" env:"TOOLGATE_SERVICE_ACCOUNT_NAMESPACES"`

	// The path of the Tink keyset that encrypts user secrets, in the JSON
	// keyset format of Tink. Mount it from a Kubernetes Secret. It is never
	// stored in PostgreSQL. The gateway serves the secrets API
	// (`/api/v1/secrets`) only when this is set. `toolgate admin rewrap`
	// needs it too. See [User secrets](user-secrets.md).
	SecretsKeysetFile string `json:"secretsKeysetFile" env:"TOOLGATE_SECRETS_KEYSET_FILE"`

	// The namespace of the Lease that picks the one replica that runs the
	// runtime reconciler, normally the gateway's own namespace. The chart
	// sets it. If it is empty, the gateway runs without Kubernetes: it
	// serves requests but never starts or stops MCP servers.
	LeaseNamespace string `json:"leaseNamespace" env:"TOOLGATE_LEASE_NAMESPACE"`
}

// Default returns the configuration used for every field that is not set.
func Default() Config {
	return Config{
		ListenAddress:          ":8080",
		MetricsListenAddress:   ":9090",
		ShutdownTimeout:        Duration(30 * time.Second),
		LogLevel:               "info",
		OIDCEmailClaim:         "email",
		OIDCEmailVerifiedClaim: "email_verified",
		OIDCGroupsClaim:        "groups",
		OIDCCLIScopes:          []string{"openid", "email", "profile", "offline_access"},
	}
}

// Duration is a time.Duration written as a Go duration string (`30s`).
type Duration time.Duration

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("must be a duration string such as \"30s\"")
	}

	return d.parse(text)
}

// MarshalJSON writes the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// String returns the duration in Go duration syntax.
func (d Duration) String() string {
	return time.Duration(d).String()
}

func (d *Duration) parse(text string) error {
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("must be a duration such as \"30s\": %w", err)
	}

	*d = Duration(parsed)

	return nil
}

// Load reads the configuration: defaults, then the YAML file at path (when
// path is not empty), then environment variables looked up with getenv. It
// returns an error naming every problem it found.
func Load(path string, getenv func(string) string) (Config, error) {
	cfg := Default()

	if path != "" {
		if err := loadFile(path, &cfg); err != nil {
			return Config{}, err
		}
	}

	var problems []error

	problems = append(problems, applyEnv(&cfg, getenv)...)
	problems = append(problems, cfg.Validate()...)

	if len(problems) > 0 {
		return Config{}, errors.Join(problems...)
	}

	return cfg, nil
}

func loadFile(path string, cfg *Config) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("configuration file: %w", err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return fmt.Errorf("configuration file %s: %w", path, err)
	}

	if len(data) > maxFileSize {
		return fmt.Errorf("configuration file %s: larger than %d bytes", path, maxFileSize)
	}

	if err := yaml.UnmarshalStrict(data, cfg); err != nil {
		return fmt.Errorf("configuration file %s: %w", path, err)
	}

	return nil
}

// applyEnv overrides fields from the environment variables named in their
// `env` tags.
func applyEnv(cfg *Config, getenv func(string) string) []error {
	var problems []error

	value := reflect.ValueOf(cfg).Elem()
	fields := value.Type()

	for index := range fields.NumField() {
		field := fields.Field(index)

		name := field.Tag.Get("env")
		if name == "" {
			continue
		}

		raw := getenv(name)
		if raw == "" {
			continue
		}

		target := value.Field(index)

		switch target.Addr().Interface().(type) {
		case *Duration:
			duration := target.Addr().Interface().(*Duration)
			if err := duration.parse(raw); err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", name, err))
			}
		case *string:
			target.SetString(raw)
		case *[]string:
			var list []string

			for _, item := range strings.Split(raw, ",") {
				if item = strings.TrimSpace(item); item != "" {
					list = append(list, item)
				}
			}

			target.Set(reflect.ValueOf(list))
		default:
			problems = append(problems, fmt.Errorf("%s: unsupported field type %s", name, field.Type))
		}
	}

	return problems
}

// Validate returns one error for each invalid field, naming its YAML key.
func (c Config) Validate() []error {
	var problems []error

	problems = append(problems, validateAddress("listenAddress", c.ListenAddress)...)
	problems = append(problems, validateAddress("metricsListenAddress", c.MetricsListenAddress)...)

	// Two listeners on port 0 each get their own free port, so they don't clash.
	if c.ListenAddress != "" && c.ListenAddress == c.MetricsListenAddress && !strings.HasSuffix(c.ListenAddress, ":0") {
		problems = append(problems, errors.New("metricsListenAddress: must differ from listenAddress"))
	}

	if c.DatabaseURL == "" {
		problems = append(problems, errors.New("databaseURL: required"))
	} else if _, err := pgxpool.ParseConfig(c.DatabaseURL); err != nil {
		// The parse error can echo the connection string, which may hold a
		// password, so it is not included.
		problems = append(problems, errors.New("databaseURL: not a valid PostgreSQL connection string"))
	}

	if c.ShutdownTimeout <= 0 {
		problems = append(problems, errors.New("shutdownTimeout: must be greater than zero"))
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Errorf("logLevel: %q is not one of debug, info, warn, error", c.LogLevel))
	}

	if c.OTLPEndpoint != "" {
		endpoint, err := url.Parse(c.OTLPEndpoint)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
			problems = append(problems, fmt.Errorf("otlpEndpoint: %q is not an http or https URL", c.OTLPEndpoint))
		}
	}

	if c.OIDCIssuerURL == "" {
		problems = append(problems, errors.New("oidcIssuerURL: required"))
	} else if issuer, err := url.Parse(c.OIDCIssuerURL); err != nil || (issuer.Scheme != "http" && issuer.Scheme != "https") || issuer.Host == "" {
		problems = append(problems, fmt.Errorf("oidcIssuerURL: %q is not an http or https URL", c.OIDCIssuerURL))
	}

	if c.OIDCAudience == "" {
		problems = append(problems, errors.New("oidcAudience: required"))
	}

	for key, value := range map[string]string{
		"oidcEmailClaim":         c.OIDCEmailClaim,
		"oidcEmailVerifiedClaim": c.OIDCEmailVerifiedClaim,
		"oidcGroupsClaim":        c.OIDCGroupsClaim,
	} {
		if value == "" {
			problems = append(problems, fmt.Errorf("%s: must not be empty", key))
		}
	}

	if len(c.OIDCCLIScopes) == 0 {
		problems = append(problems, errors.New("oidcCLIScopes: must not be empty"))
	}

	for _, namespace := range c.ServiceAccountNamespaces {
		if invalid := validation.IsDNS1123Label(namespace); len(invalid) > 0 {
			problems = append(problems, fmt.Errorf("serviceAccountNamespaces: %q is not a namespace name", namespace))
		}
	}

	if c.LeaseNamespace != "" {
		if invalid := validation.IsDNS1123Label(c.LeaseNamespace); len(invalid) > 0 {
			problems = append(problems, fmt.Errorf("leaseNamespace: %q is not a namespace name", c.LeaseNamespace))
		}
	}

	return problems
}

func validateAddress(key, address string) []error {
	if address == "" {
		return []error{fmt.Errorf("%s: required", key)}
	}

	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return []error{fmt.Errorf("%s: %q is not a host:port address", key, address)}
	}

	if number, err := strconv.Atoi(port); err != nil || number < 0 || number > 65535 {
		return []error{fmt.Errorf("%s: %q has an invalid port", key, address)}
	}

	return nil
}
