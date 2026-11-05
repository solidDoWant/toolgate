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
// +kubebuilder:validation:XValidation:rule="!has(self.__namespace__) || self.__namespace__ != 'shared' || self.scope == 'shared'",message="namespace: shared is allowed only with scope: shared"
type MCPServerTemplateSpec struct {
	// Description is shown to users and the model in the catalog.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Scope says who shares an instance: per-session, per-user or shared.
	// +required
	// +kubebuilder:validation:XValidation:rule="self in ['per-session', 'per-user', 'shared']",message="scope must be one of per-session, per-user, shared"
	Scope string `json:"scope"`

	// Namespace says where instances run: per-instance (each in its own
	// namespace) or shared (one configured namespace, scope: shared only).
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

	// Endpoint is where the instance serves MCP.
	// +required
	Endpoint Endpoint `json:"endpoint"`

	// Resources are the Kubernetes objects applied for each instance.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:XPreserveUnknownFields
	Resources []runtime.RawExtension `json:"resources,omitempty"`
}

// Access lists the identities that may use a server, and the environments
// it may be used from.
type Access struct {
	// Users are OIDC subjects or verified emails.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Users []string `json:"users,omitempty"`

	// Groups are OIDC group names.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Groups []string `json:"groups,omitempty"`

	// ServiceAccounts are system:serviceaccount:<namespace>:<name> names.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=320
	ServiceAccounts []string `json:"serviceAccounts,omitempty"`

	// Environments says which attestation a request needs: any, opt-in
	// or attested.
	// +optional
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
	// approval (phase 2).
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == 'required'",message="approval must be required or omitted"
	Approval string `json:"approval,omitempty"`

	// Blocked refuses every call to the tool.
	// +optional
	Blocked bool `json:"blocked,omitempty"`
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

	// Path is the HTTP path of the MCP endpoint.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Path string `json:"path,omitempty"`
}

// MCPServerTemplateList is a list of MCPServerTemplate.
//
// +kubebuilder:object:root=true
type MCPServerTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MCPServerTemplate `json:"items"`
}
