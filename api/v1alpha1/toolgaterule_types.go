package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolgateRule is a standing block rule: a request matching it is refused.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.spec.expires`
type ToolgateRule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ToolgateRuleSpec `json:"spec"`
	// +optional
	Status Status `json:"status,omitempty"`
}

// ToolgateRuleSpec says what the rule matches and why it refuses.
type ToolgateRuleSpec struct {
	// Match lists the fields a request must match. Every listed field must
	// match; an omitted field matches anything.
	// +optional
	Match RuleMatch `json:"match,omitempty"`

	// Reason is shown in the refusal.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason"`

	// Expires is when the rule stops applying.
	// +optional
	Expires *metav1.Time `json:"expires,omitempty"`
}

// RuleMatch lists the request fields a rule matches. List entries are
// globs where * matches any run of characters, including /.
type RuleMatch struct {
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Principals []string `json:"principals,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Groups []string `json:"groups,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=512
	Workspaces []string `json:"workspaces,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Sessions []string `json:"sessions,omitempty"`
	// Attestation is attested or user.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self in ['attested', 'user']",message="attestation must be one of attested, user"
	Attestation string `json:"attestation,omitempty"`
	// Servers are server names.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Servers []string `json:"servers,omitempty"`
	// Tools are proxied tool names, <server>__<tool>.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Tools []string `json:"tools,omitempty"`
}

// ToolgateRuleList is a list of ToolgateRule.
//
// +kubebuilder:object:root=true
type ToolgateRuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolgateRule `json:"items"`
}
