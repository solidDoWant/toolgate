package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// MCPServerTemplate is one catalog entry: an MCP server toolgate can start
// for a session, a user or everyone. Its name is the server name.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcpt
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z][a-z0-9-]{0,30}$')",message="the name is the server name and must match ^[a-z][a-z0-9-]{0,30}$"
type MCPServerTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec MCPServerTemplateSpec `json:"spec"`
	// +optional
	Status Status `json:"status,omitempty"`
}

// MCPServerTemplateSpec describes the server and who may use it.
//
// +kubebuilder:validation:XValidation:rule="!has(self.__namespace__) || self.__namespace__ != 'shared' || !has(self.scope) || self.scope == 'shared'",message="namespace: shared is allowed only with scope: shared"
// +kubebuilder:validation:XValidation:rule="!has(self.remote) || (!has(self.endpoint) && !has(self.resources) && !has(self.healthCheck))",message="remote cannot be set together with endpoint, resources or healthCheck"
// +kubebuilder:validation:XValidation:rule="has(self.remote) || has(self.endpoint)",message="endpoint is required unless remote is set"
type MCPServerTemplateSpec struct {
	// Description is shown to users and the model in the catalog.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Scope says who shares an instance: per-session, per-user or shared.
	// When it is unset, toolgate uses the broadest scope the template's
	// variables allow. When it is set, it must be that scope or narrower.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self in ['per-session', 'per-user', 'shared']",message="scope must be one of per-session, per-user, shared"
	Scope string `json:"scope,omitempty"`

	// Namespace says where instances run: per-instance (each in its own
	// namespace) or shared (one configured namespace, effective scope
	// shared only).
	// +optional
	// +kubebuilder:default=per-instance
	// +kubebuilder:validation:XValidation:rule="self in ['per-instance', 'shared']",message="namespace must be one of per-instance, shared"
	Namespace string `json:"namespace,omitempty"`

	// IdleStop is how long a per-user or shared instance runs with no
	// calls before it stops. 0 keeps a shared instance running.
	// +optional
	IdleStop *metav1.Duration `json:"idleStop,omitempty"`

	// StartTimeout is how long an instance may take to become ready.
	// +optional
	StartTimeout *metav1.Duration `json:"startTimeout,omitempty"`

	// Access lists who may see and enable the server.
	// +optional
	Access Access `json:"access,omitempty"`

	// Tools holds per-tool overrides, keyed by the tool's own name.
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	Tools map[string]ToolOverride `json:"tools,omitempty"`

	// Start says when an instance starts: onCall (the default) when a call
	// needs it, or onEnable when a session enables the server.
	// +optional
	// +kubebuilder:default=onCall
	// +kubebuilder:validation:XValidation:rule="self in ['onCall', 'onEnable']",message="start must be one of onCall, onEnable"
	Start string `json:"start,omitempty"`

	// DeclaredTools is the admin's own list of the server's tools. It is
	// shown while no instance runs, and the gateway does not learn the
	// list from instances.
	// +optional
	// +kubebuilder:validation:MaxItems=512
	// +listType=map
	// +listMapKey=name
	DeclaredTools []DeclaredTool `json:"declaredTools,omitempty"`

	// ToolFilter hides tools: it lists the upstream names to expose
	// (allow) or to hide (deny).
	// +optional
	ToolFilter *ToolFilter `json:"toolFilter,omitempty"`

	// Endpoint is where the instance serves MCP. It is required unless
	// Remote is set.
	// +optional
	Endpoint Endpoint `json:"endpoint,omitzero"`

	// Resources are the Kubernetes objects applied for each instance.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:XPreserveUnknownFields
	Resources []runtime.RawExtension `json:"resources,omitempty"`

	// HealthCheck is a custom HTTP check of a running instance. Without
	// it, only the MCP ping runs.
	// +optional
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`

	// Remote makes the template a remote server: toolgate starts nothing
	// and connects to an MCP server that already runs elsewhere. It
	// excludes endpoint, resources and healthCheck.
	// +optional
	Remote *RemoteServer `json:"remote,omitempty"`
}

// RemoteServer is an MCP server toolgate connects to but doesn't start.
type RemoteServer struct {
	// URL is the server's Streamable HTTP endpoint: https://, or http://
	// only to a cluster-internal host (ending in .svc or
	// .svc.cluster.local).
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`

	// Headers are sent with every request, keyed by header name. Values
	// may use ${secret:NAME} and ${principal.*}, filled in for each user.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(key, size(key) <= 256 && size(self[key]) <= 4096)",message="each header name must be at most 256 characters and each value at most 4096"
	Headers map[string]string `json:"headers,omitempty"`

	// Timeout bounds each request to the server, 1s to 5m.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('5m')",message="timeout must be between 1s and 5m"
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// HealthCheck is an HTTP check toolgate runs against a ready instance. While
// it fails, the instance's tools are withdrawn and calls are refused.
//
// +kubebuilder:validation:XValidation:rule="duration(self.timeout) < duration(self.interval)",message="timeout must be less than interval"
// +kubebuilder:validation:XValidation:rule="duration(self.interval) >= duration('10s') && duration(self.interval) <= duration('10m')",message="interval must be between 10s and 10m"
// +kubebuilder:validation:XValidation:rule="duration(self.timeout) >= duration('1s') && duration(self.timeout) <= duration('60s')",message="timeout must be between 1s and 60s"
type HealthCheck struct {
	// HTTP is the request to make.
	// +required
	HTTP HealthCheckHTTP `json:"http"`

	// Interval is the time between checks.
	// +optional
	// +kubebuilder:default="30s"
	Interval metav1.Duration `json:"interval,omitempty"`

	// Timeout is how long one check may take.
	// +optional
	// +kubebuilder:default="5s"
	Timeout metav1.Duration `json:"timeout,omitempty"`

	// FailureThreshold is the number of failures in a row that make the
	// instance unhealthy.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// SuccessThreshold is the number of passes in a row that make the
	// instance healthy again.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	SuccessThreshold int32 `json:"successThreshold,omitempty"`

	// OnFailure says how an unhealthy instance is repaired: replace,
	// reapply or none.
	// +optional
	// +kubebuilder:default=replace
	// +kubebuilder:validation:XValidation:rule="self in ['replace', 'reapply', 'none']",message="onFailure must be one of replace, reapply, none"
	OnFailure string `json:"onFailure,omitempty"`

	// MaxRepairs is the number of repairs allowed within 30 minutes.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	MaxRepairs int32 `json:"maxRepairs,omitempty"`
}

// HealthCheckHTTP is the GET request of a health check.
type HealthCheckHTTP struct {
	// Service is the name of a Service among the template's resources. It
	// is rendered like endpoint.service.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Service string `json:"service"`

	// Port is the Service port.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Path is the HTTP path. It is not rendered.
	// +required
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="self.startsWith('/')",message="path must start with /"
	Path string `json:"path"`

	// ExpectedStatus is the status that counts as a pass.
	// +optional
	// +kubebuilder:default=200
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=599
	ExpectedStatus int32 `json:"expectedStatus,omitempty"`
}

// Access lists the identities that may use a server, and the environments
// it may be used from.
type Access struct {
	// Users are glob patterns matched against the OIDC subject and the
	// verified email of a user identity, such as alice@example.com,
	// *@example.com, or * for every signed-in user. Escape a literal *, ?,
	// [, ], {, } or \ with a backslash. Service accounts and SPIFFE
	// identities never match them.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Users []string `json:"users,omitempty"`

	// Groups are glob patterns matched against each OIDC group of a user
	// identity. * matches any user with at least one group, not every user.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Groups []string `json:"groups,omitempty"`

	// ServiceAccounts are system:serviceaccount:<namespace>:<name> names.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=320
	ServiceAccounts []string `json:"serviceAccounts,omitempty"`

	// SPIFFEIDs are glob patterns, such as
	// spiffe://coder.example.com/owner/*/workspace/*, matched against the
	// SPIFFE ID of a JWT-SVID sign-in. Only SPIFFE identities match them.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=2048
	SPIFFEIDs []string `json:"spiffeIDs,omitempty"`

	// AllUsers gives every identity of kind user access. It is never read
	// from a template: only the gateway's built-in server sets it.
	AllUsers bool `json:"-"`

	// Environments says which attestation a request needs: any, opt-in
	// or attested. It defaults to opt-in.
	// +optional
	// +kubebuilder:default=opt-in
	// +kubebuilder:validation:XValidation:rule="self in ['any', 'opt-in', 'attested']",message="environments must be one of any, opt-in, attested"
	Environments string `json:"environments,omitempty"`
}

// ToolOverride changes the policy for one tool of a server.
type ToolOverride struct {
	// Environments overrides the server's access.environments for this tool.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self in ['any', 'opt-in', 'attested']",message="environments must be one of any, opt-in, attested"
	Environments string `json:"environments,omitempty"`

	// Approval set to required makes each call wait for a person's
	// approval.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == 'required'",message="approval must be required or omitted"
	Approval string `json:"approval,omitempty"`

	// Blocked refuses every call to the tool.
	// +optional
	Blocked bool `json:"blocked,omitempty"`

	// Name is the name the tool is exposed under, instead of its own.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]{1,128}$`
	Name string `json:"name,omitempty"`

	// Description replaces the tool's description.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Description string `json:"description,omitempty"`

	// InputDescriptions maps a top-level property of the tool's input
	// schema to the description it gets.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(key, size(self[key]) <= 4096)",message="each input description must be at most 4096 characters"
	InputDescriptions map[string]string `json:"inputDescriptions,omitempty"`
}

// DeclaredTool is one tool of the server, as the admin states it.
type DeclaredTool struct {
	// Name is the tool's own name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// Description is the tool's description.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Description string `json:"description,omitempty"`

	// InputSchema is the JSON Schema of the tool's input, an object.
	// +optional
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	InputSchema *runtime.RawExtension `json:"inputSchema,omitempty"`
}

// ToolFilter lists tool names to expose or to hide. Exactly one of Allow and
// Deny is set.
//
// +kubebuilder:validation:XValidation:rule="has(self.allow) != has(self.deny)",message="set exactly one of allow and deny"
type ToolFilter struct {
	// Allow lists the upstream names exposed. Every other tool is hidden.
	// +optional
	// +kubebuilder:validation:MaxItems=512
	// +kubebuilder:validation:items:MaxLength=128
	Allow []string `json:"allow,omitempty"`

	// Deny lists the upstream names hidden.
	// +optional
	// +kubebuilder:validation:MaxItems=512
	// +kubebuilder:validation:items:MaxLength=128
	Deny []string `json:"deny,omitempty"`
}

// Endpoint is the Service and path an instance serves MCP on.
type Endpoint struct {
	// Service is the name of a Service among the template's resources.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Service string `json:"service"`

	// Port is the Service port.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Path is the HTTP path of the MCP endpoint. It defaults to /mcp; set / to
	// use the server root.
	// +optional
	// +kubebuilder:default=/mcp
	// +kubebuilder:validation:MaxLength=1024
	Path string `json:"path,omitempty"`
}

// DefaultEndpointPath is the endpoint path used when a template leaves it unset.
// The API server fills it in through the CRD default; code that reads a
// template that skipped defaulting applies it too.
const DefaultEndpointPath = "/mcp"

// MCPServerTemplateList is a list of MCPServerTemplate.
//
// +kubebuilder:object:root=true
type MCPServerTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServerTemplate `json:"items"`
}
