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

	// The namespaces whose `MCPServerTemplate` and `ToolgateRule` objects
	// the gateway reads, such as `[toolgate-catalog]`. In the environment
	// variable, separate the names with commas. Objects in other namespaces
	// are ignored. If the list is empty, the gateway reads only
	// `leaseNamespace`. The gateway reads no catalog without
	// `leaseNamespace`.
	CatalogNamespaces []string `json:"catalogNamespaces" env:"TOOLGATE_CATALOG_NAMESPACES"`

	// The labels of the gateway's own pods, as `key=value` items, such as
	// `[app.kubernetes.io/name=toolgate]`. In the environment variable,
	// separate the items with commas. Each MCP server's network policy
	// accepts connections only from pods in `leaseNamespace` with these
	// labels. Required with `leaseNamespace`. The chart sets it.
	GatewayPodLabels []string `json:"gatewayPodLabels" env:"TOOLGATE_GATEWAY_POD_LABELS"`

	// The namespace where the MCP servers of `namespace: shared` templates
	// run, all together. The chart sets it from
	// `config.sharedNamespace.name`. The namespace must exist and carry the
	// labels `toolgate.soliddowant.dev/writable: "true"` and
	// `pod-security.kubernetes.io/enforce: restricted`. The gateway never
	// creates, labels or deletes it. It must not start with `tg-`, the prefix
	// of the namespaces the gateway makes for single servers. If it is empty,
	// every `namespace: shared` template is invalid.
	SharedNamespace string `json:"sharedNamespace" env:"TOOLGATE_SHARED_NAMESPACE"`

	// The maximum number of MCP server instances that run for one user at
	// the same time. Per-session and per-user instances count for the user
	// who started them. Shared instances do not count. When a user is at the
	// limit, an enable that must start a new instance fails. An enable of a
	// server that already runs for the user still works. At least 1. The
	// chart sets it from `config.limits.instancesPerUser`.
	InstanceLimitPerUser int `json:"instanceLimitPerUser" env:"TOOLGATE_INSTANCE_LIMIT_PER_USER"`

	// The maximum number of MCP server instances that the gateway runs at the
	// same time, for all users and shared instances together. At the limit,
	// an enable that must start a new instance fails. At least 1. The chart
	// sets it from `config.limits.instances`.
	InstanceLimit int `json:"instanceLimit" env:"TOOLGATE_INSTANCE_LIMIT"`

	// How long a session can go without requests before the gateway ends
	// it. Ending a session disables all of its servers, so its per-session
	// MCP servers stop. A later request with the same session ID starts the
	// session again with no servers enabled. The gateway checks every 30
	// seconds. Write it as a Go duration, such as `2h`. It must be more than
	// zero. The chart sets it from `config.sessionIdleTimeout`.
	SessionIdleTimeout Duration `json:"sessionIdleTimeout" env:"TOOLGATE_SESSION_IDLE_TIMEOUT"`

	// The URL where users reach the gateway in a browser, such as
	// `https://toolgate.example.com`. Required when `oidcUIClientID` is set.
	// It must be `https://`; `http://localhost` and `http://127.0.0.1` are
	// accepted for tests only. The web UI's sign-in redirect URI is this URL
	// plus `/ui/auth/callback`.
	PublicURL string `json:"publicURL" env:"TOOLGATE_PUBLIC_URL"`

	// The client ID that the web UI signs users in with, using the
	// authorization code grant with PKCE. It is a separate client from
	// `oidcCLIClientID`, and the IdP must allow the redirect URI `publicURL`
	// plus `/ui/auth/callback`. Both clients must give a user the same
	// subject (`sub`). The web UI is on only when this is set.
	OIDCUIClientID string `json:"oidcUIClientID" env:"TOOLGATE_OIDC_UI_CLIENT_ID"`

	// The path of a file that holds the web UI client's secret. Mount it from
	// a Kubernetes Secret. If it is empty, the web UI is a public client
	// (without a secret). PKCE is used in both cases.
	OIDCUIClientSecretFile string `json:"oidcUIClientSecretFile" env:"TOOLGATE_OIDC_UI_CLIENT_SECRET_FILE"`

	// The scopes that the web UI asks for at sign-in. In the environment
	// variable, separate the scopes with commas. The IdP must put the email
	// and groups claims in the ID token.
	OIDCUIScopes []string `json:"oidcUIScopes" env:"TOOLGATE_OIDC_UI_SCOPES"`

	// The group whose members are web UI admins, checked against the groups
	// from the user's sign-in. Group changes take effect at the latest when
	// the 8-hour UI session ends. If it is empty, nobody is an admin.
	AdminGroup string `json:"adminGroup" env:"TOOLGATE_ADMIN_GROUP"`
}

// WatchedNamespaces returns the namespaces whose catalog objects the gateway
// reads: CatalogNamespaces, or LeaseNamespace when that is empty.
func (c Config) WatchedNamespaces() []string {
	if len(c.CatalogNamespaces) > 0 {
		return c.CatalogNamespaces
	}

	if c.LeaseNamespace == "" {
		return nil
	}

	return []string{c.LeaseNamespace}
}

// PodLabels returns GatewayPodLabels as a map. Call it on a validated
// configuration.
func (c Config) PodLabels() map[string]string {
	labels := make(map[string]string, len(c.GatewayPodLabels))

	for _, item := range c.GatewayPodLabels {
		key, value, _ := strings.Cut(item, "=")
		labels[key] = value
	}

	return labels
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
		OIDCUIScopes:           []string{"openid", "email", "profile"},
		InstanceLimitPerUser:   20,
		InstanceLimit:          200,
		SessionIdleTimeout:     Duration(2 * time.Hour),
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
		case *int:
			number, err := strconv.Atoi(raw)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: %q is not a whole number", name, raw))

				continue
			}

			target.SetInt(int64(number))
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

		if len(c.GatewayPodLabels) == 0 {
			problems = append(problems, errors.New("gatewayPodLabels: required with leaseNamespace"))
		}
	}

	if c.SharedNamespace != "" {
		if invalid := validation.IsDNS1123Label(c.SharedNamespace); len(invalid) > 0 {
			problems = append(problems, fmt.Errorf("sharedNamespace: %q is not a namespace name", c.SharedNamespace))
		} else if strings.HasPrefix(c.SharedNamespace, "tg-") {
			problems = append(problems, fmt.Errorf("sharedNamespace: %q must not start with tg-, the prefix of instance namespaces", c.SharedNamespace))
		}
	}

	for _, namespace := range c.CatalogNamespaces {
		if invalid := validation.IsDNS1123Label(namespace); len(invalid) > 0 {
			problems = append(problems, fmt.Errorf("catalogNamespaces: %q is not a namespace name", namespace))
		}
	}

	for _, item := range c.GatewayPodLabels {
		key, value, found := strings.Cut(item, "=")
		if !found || len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
			problems = append(problems, fmt.Errorf("gatewayPodLabels: %q is not a key=value label", item))
		}
	}

	if c.InstanceLimitPerUser < 1 {
		problems = append(problems, errors.New("instanceLimitPerUser: must be at least 1"))
	}

	if c.InstanceLimit < 1 {
		problems = append(problems, errors.New("instanceLimit: must be at least 1"))
	}

	if c.SessionIdleTimeout <= 0 {
		problems = append(problems, errors.New("sessionIdleTimeout: must be greater than zero"))
	}

	problems = append(problems, c.validateUI()...)

	return problems
}

// UIEnabled reports whether the web UI is on: oidcUIClientID is set.
func (c Config) UIEnabled() bool {
	return c.OIDCUIClientID != ""
}

func (c Config) validateUI() []error {
	var problems []error

	if c.PublicURL != "" {
		if err := validatePublicURL(c.PublicURL); err != nil {
			problems = append(problems, err)
		}
	}

	if !c.UIEnabled() {
		return problems
	}

	if c.PublicURL == "" {
		problems = append(problems, errors.New("publicURL: required with oidcUIClientID"))
	}

	return problems
}

// validatePublicURL accepts an https URL, or http on localhost or 127.0.0.1
// for tests: the UI's `__Host-` cookies need a secure origin.
func validatePublicURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("publicURL: %q is not a URL with only a scheme, host and port", raw)
	}

	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if host := parsed.Hostname(); host == "localhost" || host == "127.0.0.1" {
			return nil
		}
	}

	return fmt.Errorf("publicURL: %q must be https://, or http://localhost or http://127.0.0.1 for tests", raw)
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
