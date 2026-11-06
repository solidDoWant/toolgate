// Package config loads and validates the gateway's configuration from an
// optional YAML file and from environment variables.
//
// The doc comments on the Config fields are the configuration reference:
// `make generate-docs` turns them into docs/admin/configuration.md.
package config

import (
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/solidDoWant/toolgate/internal/auth"

	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"regexp"
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

	// Whether the gateway creates and updates its tables when it starts. If
	// it is `false`, the gateway does not change the schema. Before it opens
	// its listeners, it checks that the database schema is at the version it
	// needs. If the schema is older, or has no tables yet, the gateway logs a
	// message and exits with code 1. Run `toolgate migrate`, or let the
	// chart's migration Job run, before you start the gateway. Use `false`
	// when the gateway's database user has data rights only. A schema that is
	// newer than the gateway (after a rollback) is only logged.
	MigrateOnStart bool `json:"migrateOnStart" env:"TOOLGATE_MIGRATE_ON_START"`

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

	// When it is `true`, the gateway accepts OpenTelemetry data from signed-in
	// clients at `POST /v1/traces`, `/v1/metrics` and `/v1/logs`. It forwards
	// the data to `otlpEndpoint`, so `otlpEndpoint` is required. The gateway
	// removes the identity attributes that the client sent. These are the
	// attributes whose name starts with `user_`, `enduser_` or `workspace_`
	// after each character outside `[A-Za-z0-9]` changes to `_` and the name
	// changes to lower case. Then the gateway sets `user.id`, `enduser.id`,
	// `user.email` and `workspace.id` from the verified identity. A request
	// body can be 4 MiB at most, and it must arrive within 10 seconds, or the
	// gateway answers 408. Each replica handles 4 requests at the same time at
	// most. When it is `false`, these paths answer 404.
	OTLPIngest bool `json:"otlpIngest" env:"TOOLGATE_OTLP_INGEST"`

	// When it is `true`, the gateway keeps prompt and tool content in the
	// OpenTelemetry data that clients send to it. Prompts, tool inputs and
	// tool output are examples of this content. Clients add it when users set
	// `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS` or
	// `OTEL_LOG_TOOL_CONTENT`. When it is `false`, the gateway removes it.
	// Set it to `true` only when the telemetry backend is allowed to hold user
	// content. The gateway then sends that content to the backend.
	TelemetryAllowContent bool `json:"telemetry.allowContent" env:"TOOLGATE_TELEMETRY_ALLOW_CONTENT"`

	// The issuer URL of the OIDC identity provider (IdP) that users sign in
	// with, such as `https://idp.example.com/realms/main`. Required. The
	// gateway reads the IdP's discovery document and signing keys from it,
	// and accepts only access tokens this issuer signed.
	OIDCIssuerURL string `json:"oidcIssuerURL" env:"TOOLGATE_OIDC_ISSUER_URL"`

	// The audiences that users' access tokens may be for, such as `toolgate`
	// or `[toolgate, coder-toolgate]`: one value or a list of 1 to 8. Each
	// is the client ID of an OIDC client at the `oidcIssuerURL` provider,
	// and is at most 256 characters long. Required. In the environment
	// variable, separate the audiences with commas. The gateway accepts a
	// token that has at least one of these audiences, and refuses a token
	// for none of them. Every audience is a client whose access tokens the
	// gateway accepts, so list only clients made for toolgate.
	OIDCAudience Audiences `json:"oidcAudience" env:"TOOLGATE_OIDC_AUDIENCE"`

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
	// it. An ended session keeps its list of enabled servers for
	// `sessionResumeWindow`, but its per-session MCP servers stop. A later
	// request with the same session ID starts the session again with no
	// servers enabled. The gateway checks every 30 seconds. Write it as a Go
	// duration, such as `2h`. It must be more than zero. The chart sets it
	// from `config.sessionIdleTimeout`.
	SessionIdleTimeout Duration `json:"sessionIdleTimeout" env:"TOOLGATE_SESSION_IDLE_TIMEOUT"`

	// How long the gateway keeps the list of enabled servers of a session
	// that ended. After this time, the gateway deletes the list and records
	// a `session.forget` audit event. The gateway checks every 30 seconds.
	// Write it as a Go duration, such as `24h`. It must be from `1h` to
	// `168h` (7 days). The chart sets it from `config.sessionResumeWindow`.
	SessionResumeWindow Duration `json:"sessionResumeWindow" env:"TOOLGATE_SESSION_RESUME_WINDOW"`

	// The URL where users reach the gateway in a browser, such as
	// `https://toolgate.example.com`. Required when `oidcUIClientID` is set.
	// It must be `https://`; `http://localhost` and `http://127.0.0.1` are
	// accepted for tests only. The web UI's sign-in redirect URI is this URL
	// plus `/ui/auth/callback`.
	PublicURL string `json:"publicURL" env:"TOOLGATE_PUBLIC_URL"`

	// The URL of the MCP endpoint as MCP clients on the internet reach it,
	// such as `https://mcp.example.com/mcp`. The gateway names it as the
	// resource in its OAuth protected resource metadata, so that standard
	// MCP clients, such as claude.ai connectors, can sign in. It must be
	// `https://`; `http://localhost` and `http://127.0.0.1` are accepted for
	// tests only. If it is empty, it is `publicURL` plus `/mcp`. If both are
	// empty, the gateway serves no metadata. The chart sets it from
	// `config.mcpPublicURL`.
	MCPPublicURL string `json:"mcpPublicURL" env:"TOOLGATE_MCP_PUBLIC_URL"`

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
	// the 8-hour UI session ends. Members who sign in as users, not service
	// accounts, also see the audit events of all users in `toolgate audit
	// tail`; there the group is checked on each request. If it is empty,
	// nobody is an admin.
	AdminGroup string `json:"adminGroup" env:"TOOLGATE_ADMIN_GROUP"`

	// How long a call that needs approval waits for a person to approve it,
	// and how long an approved request stays usable. The gateway marks a
	// waiting request expired 1 minute after this time has passed. Write it
	// as a Go duration, such as `10m`. It must be from 1 minute to 1 hour.
	// The chart sets it from `config.ui.approvalTimeout`.
	ApprovalTimeout Duration `json:"approvalTimeout" env:"TOOLGATE_APPROVAL_TIMEOUT"`

	// Whether the gateway stores audit payloads: the arguments of each tool
	// call and approval request, and each tool result. A payload under 64 KiB
	// (65,536 bytes) is stored in its audit row in PostgreSQL. A larger one
	// is stored as an object in `auditS3Bucket`. If this is `false`, no
	// arguments or results are stored. If it is `true`, `auditS3Bucket` is
	// required. The chart sets it from `config.audit.payloads`.
	AuditPayloads bool `json:"auditPayloads" env:"TOOLGATE_AUDIT_PAYLOADS"`

	// Whether tool search mode is on for sessions whose client sends no
	// `X-Toolgate-Tool-Search` header. In search mode, a session lists the
	// meta tools, `toolgate_find_tools`, the tools of its enabled servers and
	// the tools it found by search. The header only changes what is listed,
	// never what is allowed. The chart sets it from `config.toolSearch`.
	ToolSearch bool `json:"toolSearch" env:"TOOLGATE_TOOL_SEARCH"`

	// The base URL of an endpoint that speaks the OpenAI embeddings API
	// (`POST <url>/v1/embeddings`), such as Hugging Face text-embeddings-
	// inference or Ollama run in the cluster. If it is set, tool search ranks
	// by meaning as well as by words, and the gateway sends the search query
	// and the tool descriptions to this endpoint. If it is empty, search uses
	// words only. It must be an `http://` or `https://` URL.
	// `toolSearchEmbeddingsModel` is then required. The chart sets it from
	// `config.toolSearchEmbeddings.url`.
	ToolSearchEmbeddingsURL string `json:"toolSearchEmbeddingsURL" env:"TOOLGATE_TOOL_SEARCH_EMBEDDINGS_URL"`

	// The model name sent to the embeddings endpoint, such as
	// `BAAI/bge-small-en-v1.5`. Changing it makes the gateway embed all tools
	// again.
	ToolSearchEmbeddingsModel string `json:"toolSearchEmbeddingsModel" env:"TOOLGATE_TOOL_SEARCH_EMBEDDINGS_MODEL"`

	// The path of a file that holds an API key for the embeddings endpoint,
	// sent as `Authorization: Bearer`. Mount it from a Kubernetes Secret.
	// Empty means no key.
	ToolSearchEmbeddingsAPIKeyFile string `json:"toolSearchEmbeddingsAPIKeyFile" env:"TOOLGATE_TOOL_SEARCH_EMBEDDINGS_API_KEY_FILE"`

	// How long one request to the embeddings endpoint may take. Write it as a
	// Go duration, such as `2s`. It must be from 100 milliseconds to 30
	// seconds. A search that runs out of time uses words only.
	ToolSearchEmbeddingsTimeout Duration `json:"toolSearchEmbeddingsTimeout" env:"TOOLGATE_TOOL_SEARCH_EMBEDDINGS_TIMEOUT"`

	// The base URL of a rerank endpoint (`POST <url>/rerank`, the Hugging
	// Face text-embeddings-inference route), such as a second instance of that
	// server that serves `BAAI/bge-reranker-base`. If it is set, each search
	// sends the query and the 20 best tools to it and returns the best 5 in
	// its order. If it is empty, there is no reranking. `toolSearchRerankModel`
	// is then required. The chart sets it from `config.toolSearchRerank.url`.
	ToolSearchRerankURL string `json:"toolSearchRerankURL" env:"TOOLGATE_TOOL_SEARCH_RERANK_URL"`

	// The model name of the rerank endpoint, such as
	// `BAAI/bge-reranker-base`.
	ToolSearchRerankModel string `json:"toolSearchRerankModel" env:"TOOLGATE_TOOL_SEARCH_RERANK_MODEL"`

	// The path of a file that holds an API key for the rerank endpoint, sent
	// as `Authorization: Bearer`. Mount it from a Kubernetes Secret. Empty
	// means no key.
	ToolSearchRerankAPIKeyFile string `json:"toolSearchRerankAPIKeyFile" env:"TOOLGATE_TOOL_SEARCH_RERANK_API_KEY_FILE"`

	// How long one request to the rerank endpoint may take. Write it as a Go
	// duration, such as `2s`. It must be from 100 milliseconds to 30 seconds.
	// A search that runs out of time returns the order without reranking.
	ToolSearchRerankTimeout Duration `json:"toolSearchRerankTimeout" env:"TOOLGATE_TOOL_SEARCH_RERANK_TIMEOUT"`

	// The URL of the S3 API that stores large audit payloads, such as
	// `https://s3.example.com`. If it is empty, the gateway uses the AWS S3
	// endpoint of `auditS3Region`. The gateway gets its S3 credentials from
	// the default chain of the AWS SDK, such as the `AWS_ACCESS_KEY_ID` and
	// `AWS_SECRET_ACCESS_KEY` environment variables. The chart sets it from
	// `config.audit.s3.endpoint`.
	AuditS3Endpoint string `json:"auditS3Endpoint" env:"TOOLGATE_AUDIT_S3_ENDPOINT"`

	// The S3 region of `auditS3Bucket`, such as `us-east-1`. The chart sets
	// it from `config.audit.s3.region`.
	AuditS3Region string `json:"auditS3Region" env:"TOOLGATE_AUDIT_S3_REGION"`

	// The S3 bucket that stores large audit payloads. Required when
	// `auditPayloads` is `true`. If the bucket has object lock, the gateway
	// locks each object in governance mode for the audit retention time.
	// Add a lifecycle rule under `auditS3Prefix` that expires current objects
	// and noncurrent versions after the retention time plus a margin. The
	// chart sets it from `config.audit.s3.bucket`.
	AuditS3Bucket string `json:"auditS3Bucket" env:"TOOLGATE_AUDIT_S3_BUCKET"`

	// The prefix of the object keys of audit payloads, such as `toolgate/`.
	// Each key is `<prefix>payloads/<yyyy>/<mm>/<dd>/<uuid>.json`. The chart
	// sets it from `config.audit.s3.prefix`.
	AuditS3Prefix string `json:"auditS3Prefix" env:"TOOLGATE_AUDIT_S3_PREFIX"`

	// Whether the gateway puts the bucket name in the URL path
	// (`https://s3.example.com/bucket/key`) instead of the host name. Many
	// S3-compatible servers need `true`. The chart sets it from
	// `config.audit.s3.pathStyle`.
	AuditS3PathStyle bool `json:"auditS3PathStyle" env:"TOOLGATE_AUDIT_S3_PATH_STYLE"`

	// The HTTP services a template's `${resolver:<name>}` gets a value from.
	// Set it in the YAML file only. Each item has a `name` (lowercase
	// letters, digits and `-`, starting with a letter, at most 63
	// characters), a `url` that must be `https://`, an `audience` for the
	// service-account token the gateway sends, a `timeout` (a Go duration,
	// default `10s`, at most `60s`) and an optional `caFile`, a PEM file of
	// the certificate authorities that sign the resolver's certificate. The
	// gateway reads the token from
	// `/var/run/secrets/toolgate/resolvers/<name>/token` for each request.
	// The chart sets it from `config.resolvers` and mounts each token.
	Resolvers []Resolver `json:"resolvers"`

	// The SPIFFE trust domains whose JWT-SVIDs may sign in. Set it in the
	// YAML file only. Each item has a `name` (the trust domain, such as
	// `coder.example.com`), exactly one of `bundleEndpointURL` (a SPIFFE
	// bundle endpoint with the `https_web` profile) and `jwksURL` (a JWKS
	// document, such as that of SPIRE's OIDC discovery provider), both
	// `https://`, and an optional `caFile`, a PEM file of private
	// certificate authorities added to the system ones. This list is the
	// only gateway-level allow-list for SPIFFE IDs. The chart sets it from
	// `config.spiffe.trustDomains`.
	SPIFFETrustDomains []SPIFFETrustDomain `json:"spiffeTrustDomains"`

	// Attested user sessions: a user's token plus a workload token from a
	// workspace pod or a SPIFFE workload, bound to that user, so the
	// session's workspace is verified. Set it in the YAML file only; the
	// chart sets it from `config.workspaceAttestation`. Off when absent: the
	// `X-Toolgate-Attestation` header is then ignored. `namespaces`
	// (required) lists the namespaces whose service-account tokens may
	// attest a user; it is separate from `serviceAccountNamespaces`. `keys`
	// names the pod label keys (annotations are read when a label is
	// missing) of `userID`, `email`, `username` and `workspace`; they
	// default to Coder's `com.coder.user.id`, `com.coder.user.email`,
	// `com.coder.user.username` and `com.coder.workspace.name`. `match.by`
	// is `email` (the pod's email must equal the user's verified email,
	// ignoring case) or `claim` (the user token's `match.claim` must be a
	// string equal to the pod's `match.key`, `userID` or `username`).
	// `workspaceID` (required) is the verified workspace, such as
	// `coder/{username}/{workspace}`; it may use `{userID}`, `{username}`
	// and `{workspace}`, must start with a literal segment that isn't
	// `claimed`, `k8s` or empty, and is at most 256 characters once
	// rendered. `spiffeIDPattern`, if set, also accepts JWT-SVIDs whose ID
	// matches it, such as
	// `spiffe://coder.example.com/coder/{username}/{workspace}`: its trust
	// domain must be in `spiffeTrustDomains`, each placeholder is one whole
	// path segment, `match.by` must be `claim`, and it must contain
	// `{<match.key>}` and every placeholder `workspaceID` uses.
	WorkspaceAttestation *WorkspaceAttestation `json:"workspaceAttestation"`
}

// WorkspaceAttestation is the workspaceAttestation setting.
type WorkspaceAttestation struct {
	Namespaces      []string                  `json:"namespaces"`
	Keys            WorkspaceAttestationKeys  `json:"keys"`
	Match           WorkspaceAttestationMatch `json:"match"`
	WorkspaceID     string                    `json:"workspaceID"`
	SPIFFEIDPattern string                    `json:"spiffeIDPattern"`
}

// WorkspaceAttestationKeys are the pod label keys of workspaceAttestation.
type WorkspaceAttestationKeys struct {
	UserID    string `json:"userID"`
	Email     string `json:"email"`
	Username  string `json:"username"`
	Workspace string `json:"workspace"`
}

// WorkspaceAttestationMatch is how workspaceAttestation compares the
// workload's owner with the user.
type WorkspaceAttestationMatch struct {
	By    string `json:"by"`
	Claim string `json:"claim"`
	Key   string `json:"key"`
}

// AttestationOptions returns the authenticator's options for
// workspaceAttestation, without a Kubernetes client, or nil when it is off.
func (c Config) AttestationOptions() *auth.AttestationOptions {
	if c.WorkspaceAttestation == nil {
		return nil
	}

	attestation := c.WorkspaceAttestation

	trustDomains := make([]string, 0, len(c.SPIFFETrustDomains))
	for _, domain := range c.SPIFFETrustDomains {
		trustDomains = append(trustDomains, domain.Name)
	}

	return &auth.AttestationOptions{
		Namespaces: attestation.Namespaces,
		Keys: auth.AttestationKeys{
			UserID:    attestation.Keys.UserID,
			Email:     attestation.Keys.Email,
			Username:  attestation.Keys.Username,
			Workspace: attestation.Keys.Workspace,
		},
		MatchBy:            attestation.Match.By,
		MatchClaim:         attestation.Match.Claim,
		MatchKey:           attestation.Match.Key,
		WorkspaceID:        attestation.WorkspaceID,
		SPIFFEIDPattern:    attestation.SPIFFEIDPattern,
		SPIFFETrustDomains: trustDomains,
	}
}

func (c Config) validateWorkspaceAttestation() []error {
	options := c.AttestationOptions()
	if options == nil {
		return nil
	}

	problems := auth.ValidateAttestation(*options)
	for index, problem := range problems {
		problems[index] = fmt.Errorf("workspaceAttestation.%w", problem)
	}

	return problems
}

// SPIFFETrustDomain is one entry of the spiffeTrustDomains setting.
type SPIFFETrustDomain struct {
	Name              string `json:"name"`
	BundleEndpointURL string `json:"bundleEndpointURL"`
	JWKSURL           string `json:"jwksURL"`
	CAFile            string `json:"caFile"`
}

func (c Config) validateSPIFFETrustDomains() []error {
	var problems []error

	seen := map[string]bool{}

	for index, domain := range c.SPIFFETrustDomains {
		where := fmt.Sprintf("spiffeTrustDomains[%d]", index)

		if trustDomain, err := spiffeid.TrustDomainFromString(domain.Name); err != nil {
			problems = append(problems, fmt.Errorf("%s.name: %q is not a SPIFFE trust domain: %w", where, domain.Name, err))
		} else if seen[trustDomain.Name()] {
			problems = append(problems, fmt.Errorf("%s.name: %q is listed twice", where, domain.Name))
		} else {
			seen[trustDomain.Name()] = true
		}

		switch {
		case (domain.BundleEndpointURL == "") == (domain.JWKSURL == ""):
			problems = append(problems, fmt.Errorf("%s: set exactly one of bundleEndpointURL and jwksURL", where))
		case domain.BundleEndpointURL != "" && !isHTTPSURL(domain.BundleEndpointURL):
			problems = append(problems, fmt.Errorf("%s.bundleEndpointURL: %q must be an https:// URL", where, domain.BundleEndpointURL))
		case domain.JWKSURL != "" && !isHTTPSURL(domain.JWKSURL):
			problems = append(problems, fmt.Errorf("%s.jwksURL: %q must be an https:// URL", where, domain.JWKSURL))
		}
	}

	return problems
}

func isHTTPSURL(value string) bool {
	target, err := url.Parse(value)

	return err == nil && target.Scheme == "https" && target.Host != ""
}

// Resolver is one entry of the resolvers setting.
type Resolver struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	Audience string   `json:"audience"`
	Timeout  Duration `json:"timeout"`
	CAFile   string   `json:"caFile"`
}

// Resolver limits (docs/design/phase-2.md §6).
const (
	DefaultResolverTimeout = Duration(10 * time.Second)
	MaxResolverTimeout     = Duration(60 * time.Second)
)

// resolverNamePattern is the form of a resolver name.
var resolverNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ResolverTimeout returns the resolver's timeout, or the default when it is
// not set.
func (r Resolver) ResolverTimeout() time.Duration {
	if r.Timeout == 0 {
		return time.Duration(DefaultResolverTimeout)
	}

	return time.Duration(r.Timeout)
}

func (c Config) validateResolvers() []error {
	var problems []error

	seen := map[string]bool{}

	for index, resolver := range c.Resolvers {
		where := fmt.Sprintf("resolvers[%d]", index)

		if !resolverNamePattern.MatchString(resolver.Name) {
			problems = append(problems, fmt.Errorf("%s.name: %q must match %s", where, resolver.Name, resolverNamePattern))
		} else if seen[resolver.Name] {
			problems = append(problems, fmt.Errorf("%s.name: %q is listed twice", where, resolver.Name))
		}

		seen[resolver.Name] = true

		if target, err := url.Parse(resolver.URL); err != nil || target.Scheme != "https" || target.Host == "" {
			problems = append(problems, fmt.Errorf("%s.url: %q must be an https:// URL", where, resolver.URL))
		}

		if resolver.Audience == "" {
			problems = append(problems, fmt.Errorf("%s.audience: required", where))
		}

		if resolver.Timeout < 0 || resolver.Timeout > MaxResolverTimeout {
			problems = append(problems, fmt.Errorf("%s.timeout: must be at most 60s", where))
		}
	}

	return problems
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
		MigrateOnStart:         true,
		OIDCEmailClaim:         "email",
		OIDCEmailVerifiedClaim: "email_verified",
		OIDCGroupsClaim:        "groups",
		OIDCCLIScopes:          []string{"openid", "email", "profile", "offline_access"},
		OIDCUIScopes:           []string{"openid", "email", "profile"},
		InstanceLimitPerUser:   20,
		InstanceLimit:          200,
		SessionIdleTimeout:     Duration(2 * time.Hour),
		SessionResumeWindow:    Duration(24 * time.Hour),
		ApprovalTimeout:        Duration(10 * time.Minute),

		ToolSearchEmbeddingsTimeout: Duration(2 * time.Second),
		ToolSearchRerankTimeout:     Duration(2 * time.Second),
	}
}

// Audience limits.
const (
	maxAudiences      = 8
	maxAudienceLength = 256
)

// Audiences is a list of OIDC audiences. In YAML it is one string or a list
// of strings, so a single-value setting is a one-item list.
type Audiences []string

// UnmarshalJSON parses one string or a list of strings.
func (a *Audiences) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = Audiences{single}

		return nil
	}

	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return errors.New("must be a string or a list of strings")
	}

	*a = list

	return nil
}

func (a Audiences) validate() []error {
	if len(a) == 0 {
		return []error{errors.New("oidcAudience: required")}
	}

	var problems []error

	if len(a) > maxAudiences {
		problems = append(problems, fmt.Errorf("oidcAudience: %d audiences, at most %d are allowed", len(a), maxAudiences))
	}

	seen := make(map[string]bool, len(a))

	for _, audience := range a {
		switch {
		case audience == "":
			problems = append(problems, errors.New("oidcAudience: an audience must not be empty"))
		case len(audience) > maxAudienceLength:
			problems = append(problems, fmt.Errorf("oidcAudience: an audience is longer than %d characters", maxAudienceLength))
		case seen[audience]:
			problems = append(problems, fmt.Errorf("oidcAudience: %q is listed more than once", audience))
		}

		seen[audience] = true
	}

	return problems
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

// LoadDatabaseURL reads only databaseURL, from the YAML file at path (when
// path is not empty) and then from TOOLGATE_DATABASE_URL. It ignores every
// other setting, so `toolgate migrate` needs no other configuration.
func LoadDatabaseURL(path string, getenv func(string) string) (string, error) {
	var file struct {
		DatabaseURL string `json:"databaseURL"`
	}

	if path != "" {
		data, err := readFile(path)
		if err != nil {
			return "", err
		}

		if err := yaml.Unmarshal(data, &file); err != nil {
			return "", fmt.Errorf("configuration file %s: %w", path, err)
		}
	}

	databaseURL := file.DatabaseURL
	if value := getenv("TOOLGATE_DATABASE_URL"); value != "" {
		databaseURL = value
	}

	if databaseURL == "" {
		return "", errors.New("databaseURL: required")
	}

	if _, err := pgxpool.ParseConfig(databaseURL); err != nil {
		// The parse error can echo the connection string, which may hold a
		// password, so it is not included.
		return "", errors.New("databaseURL: not a valid PostgreSQL connection string")
	}

	return databaseURL, nil
}

// readFile reads the configuration file, bounded by maxFileSize.
func readFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("configuration file: %w", err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("configuration file %s: %w", path, err)
	}

	if len(data) > maxFileSize {
		return nil, fmt.Errorf("configuration file %s: larger than %d bytes", path, maxFileSize)
	}

	return data, nil
}

func loadFile(path string, cfg *Config) error {
	data, err := readFile(path)
	if err != nil {
		return err
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
		case *bool:
			flag, err := strconv.ParseBool(raw)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: %q is not true or false", name, raw))

				continue
			}

			target.SetBool(flag)
		case *int:
			number, err := strconv.Atoi(raw)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: %q is not a whole number", name, raw))

				continue
			}

			target.SetInt(int64(number))
		case *Audiences:
			target.Set(reflect.ValueOf(Audiences(splitList(raw))))
		case *[]string:
			target.Set(reflect.ValueOf(splitList(raw)))
		default:
			problems = append(problems, fmt.Errorf("%s: unsupported field type %s", name, field.Type))
		}
	}

	return problems
}

// splitList splits a comma-separated environment variable, dropping empty
// items.
func splitList(raw string) []string {
	var list []string

	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			list = append(list, item)
		}
	}

	return list
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

	if c.OTLPIngest && c.OTLPEndpoint == "" {
		problems = append(problems, errors.New("otlpEndpoint: required when otlpIngest is true"))
	}

	if c.OIDCIssuerURL == "" {
		problems = append(problems, errors.New("oidcIssuerURL: required"))
	} else if issuer, err := url.Parse(c.OIDCIssuerURL); err != nil || (issuer.Scheme != "http" && issuer.Scheme != "https") || issuer.Host == "" {
		problems = append(problems, fmt.Errorf("oidcIssuerURL: %q is not an http or https URL", c.OIDCIssuerURL))
	}

	problems = append(problems, c.OIDCAudience.validate()...)

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

	if c.SessionResumeWindow < Duration(time.Hour) || c.SessionResumeWindow > Duration(7*24*time.Hour) {
		problems = append(problems, errors.New("sessionResumeWindow: must be from 1h to 168h"))
	}

	if c.ApprovalTimeout < Duration(time.Minute) || c.ApprovalTimeout > Duration(time.Hour) {
		problems = append(problems, errors.New("approvalTimeout: must be from 1m to 1h"))
	}

	problems = append(problems, c.validateToolSearchEmbeddings()...)
	problems = append(problems, c.validateToolSearchRerank()...)
	problems = append(problems, c.validateUI()...)
	problems = append(problems, c.validateResolvers()...)
	problems = append(problems, c.validateSPIFFETrustDomains()...)
	problems = append(problems, c.validateWorkspaceAttestation()...)

	if c.AuditPayloads && c.AuditS3Bucket == "" {
		problems = append(problems, errors.New("auditS3Bucket: required when auditPayloads is true"))
	}

	if c.AuditS3Endpoint != "" {
		endpoint, err := url.Parse(c.AuditS3Endpoint)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
			problems = append(problems, fmt.Errorf("auditS3Endpoint: %q is not an http or https URL", c.AuditS3Endpoint))
		}
	}

	return problems
}

// UIEnabled reports whether the web UI is on: oidcUIClientID is set.
func (c Config) UIEnabled() bool {
	return c.OIDCUIClientID != ""
}

func (c Config) validateUI() []error {
	var problems []error

	if c.MCPPublicURL != "" {
		if err := validateMCPPublicURL(c.MCPPublicURL); err != nil {
			problems = append(problems, err)
		}
	}

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
	if err != nil || strings.ContainsAny(raw, `"\\`) || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("publicURL: %q is not a URL with only a scheme, host and port", raw)
	}

	if secureOrLocal(parsed) {
		return nil
	}

	return fmt.Errorf("publicURL: %q must be https://, or http://localhost or http://127.0.0.1 for tests", raw)
}

// MCPResourceURL is the MCP endpoint's URL that the protected resource
// metadata names: mcpPublicURL, or publicURL plus /mcp. It is empty when both
// are empty.
func (c Config) MCPResourceURL() string {
	if c.MCPPublicURL != "" {
		return c.MCPPublicURL
	}

	if c.PublicURL == "" {
		return ""
	}

	return strings.TrimSuffix(c.PublicURL, "/") + "/mcp"
}

// validateMCPPublicURL accepts an https URL, or http on localhost or
// 127.0.0.1 for tests, with a path but no user info, query or fragment. A
// quote or backslash is refused: the URL goes into a quoted header value.
func validateMCPPublicURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || strings.ContainsAny(raw, `"\\`) || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("mcpPublicURL: %q is not a URL with only a scheme, host, port and path", raw)
	}

	if !secureOrLocal(parsed) {
		return fmt.Errorf("mcpPublicURL: %q must be https://, or http://localhost or http://127.0.0.1 for tests", raw)
	}

	return nil
}

// secureOrLocal reports whether parsed is https, or http on localhost or
// 127.0.0.1.
func secureOrLocal(parsed *url.URL) bool {
	switch parsed.Scheme {
	case "https":
		return true
	case "http":
		host := parsed.Hostname()

		return host == "localhost" || host == "127.0.0.1"
	}

	return false
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

// validateToolSearchEmbeddings checks the embeddings settings of tool search.
func (c Config) validateToolSearchEmbeddings() []error {
	if c.ToolSearchEmbeddingsURL == "" {
		return nil
	}

	var problems []error

	endpoint, err := url.Parse(c.ToolSearchEmbeddingsURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		problems = append(problems, errors.New("toolSearchEmbeddingsURL: must be an http:// or https:// URL"))
	}

	if c.ToolSearchEmbeddingsModel == "" {
		problems = append(problems, errors.New("toolSearchEmbeddingsModel: required when toolSearchEmbeddingsURL is set"))
	}

	if c.ToolSearchEmbeddingsTimeout < Duration(100*time.Millisecond) || c.ToolSearchEmbeddingsTimeout > Duration(30*time.Second) {
		problems = append(problems, errors.New("toolSearchEmbeddingsTimeout: must be from 100ms to 30s"))
	}

	return problems
}

// validateToolSearchRerank checks the rerank settings of tool search.
func (c Config) validateToolSearchRerank() []error {
	if c.ToolSearchRerankURL == "" {
		return nil
	}

	var problems []error

	endpoint, err := url.Parse(c.ToolSearchRerankURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		problems = append(problems, errors.New("toolSearchRerankURL: must be an http:// or https:// URL"))
	}

	if c.ToolSearchRerankModel == "" {
		problems = append(problems, errors.New("toolSearchRerankModel: required when toolSearchRerankURL is set"))
	}

	if c.ToolSearchRerankTimeout < Duration(100*time.Millisecond) || c.ToolSearchRerankTimeout > Duration(30*time.Second) {
		problems = append(problems, errors.New("toolSearchRerankTimeout: must be from 100ms to 30s"))
	}

	return problems
}
