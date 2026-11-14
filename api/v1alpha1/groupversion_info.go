// Package v1alpha1 holds the custom resources toolgate reads: catalog
// entries (MCPServerTemplate), standing block rules (ToolgateRule) and
// Claude Code settings profiles (SettingsProfile) and OAuth token exchanges
// (TokenExchange).
//
// +kubebuilder:object:generate=true
// +groupName=toolgate.soliddowant.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group and version of every toolgate resource.
var GroupVersion = schema.GroupVersion{Group: "toolgate.soliddowant.dev", Version: "v1alpha1"}

var (
	// SchemeBuilder registers the toolgate types with a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the toolgate types to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&MCPServerTemplate{}, &MCPServerTemplateList{},
		&ToolgateRule{}, &ToolgateRuleList{},
		&SettingsProfile{}, &SettingsProfileList{},
		&TokenExchange{}, &TokenExchangeList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)

	return nil
}

// Status is the status every toolgate resource has: toolgate reports
// whether it accepted the resource, and why not, in its conditions.
type Status struct {
	// Conditions report whether toolgate accepted the resource.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
