package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TokenExchange is an OAuth 2.0 token exchange (RFC 8693) that templates in
// the same namespace name in `${exchange:NAME}`. The gateway sends a user's
// access token to its tokenURL, so the URL must be on the gateway's
// allow-list for the resource to be valid.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type TokenExchange struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec TokenExchangeSpec `json:"spec"`
	// +optional
	Status Status `json:"status,omitempty"`
}

// TokenExchangeSpec says where and how the gateway exchanges a token.
type TokenExchangeSpec struct {
	// TokenURL is the IdP's token endpoint, an https:// URL on the
	// gateway's allow-list.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	TokenURL string `json:"tokenURL"`

	// ClientID is the confidential client that exchanges tokens.
	// +required
	// +kubebuilder:validation:MaxLength=256
	ClientID string `json:"clientID"`

	// ClientSecretRef names the Secret key that holds the client secret,
	// in the template secrets namespace.
	// +required
	ClientSecretRef SecretKeyRef `json:"clientSecretRef"`

	// Audience is the RFC 8693 audience parameter.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Audience string `json:"audience,omitempty"`

	// Scope is a space-separated list of scopes.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Scope string `json:"scope,omitempty"`

	// RequestedTokenType is the RFC 8693 requested_token_type URI.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	RequestedTokenType string `json:"requestedTokenType,omitempty"`

	// Timeout bounds one exchange: a Go duration, default 10s, at most 60s.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// SecretKeyRef names one key of a Secret.
type SecretKeyRef struct {
	// +required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +required
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// TokenExchangeList is a list of TokenExchange.
//
// +kubebuilder:object:root=true
type TokenExchangeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TokenExchange `json:"items"`
}
