package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SettingsProfile is a fragment of Claude Code settings handed to the
// clients its selector matches.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type SettingsProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec SettingsProfileSpec `json:"spec"`
	// +optional
	Status Status `json:"status,omitempty"`
}

// SettingsProfileSpec holds the settings and who gets them.
type SettingsProfileSpec struct {
	// Selector says which identities get the profile.
	// +optional
	Selector ProfileSelector `json:"selector,omitempty"`

	// Settings is a Claude Code settings fragment (permissions, env, hooks
	// and other keys), merged into the client's settings.json.
	// +required
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Settings runtime.RawExtension `json:"settings"`
}

// ProfileSelector lists the identities a profile applies to.
type ProfileSelector struct {
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Groups []string `json:"groups,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=256
	Principals []string `json:"principals,omitempty"`
	// Attestation is attested or user.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self in ['attested', 'user']",message="attestation must be one of attested, user"
	Attestation string `json:"attestation,omitempty"`
}

// SettingsProfileList is a list of SettingsProfile.
//
// +kubebuilder:object:root=true
type SettingsProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SettingsProfile `json:"items"`
}
