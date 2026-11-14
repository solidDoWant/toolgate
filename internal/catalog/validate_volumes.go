package catalog

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/templatescope"
)

// validatePersistentVolumes checks the template's persistentVolumes, if any.
func (v Validator) validatePersistentVolumes(spec v1alpha1.MCPServerTemplateSpec) []string {
	if len(spec.PersistentVolumes) == 0 {
		return nil
	}

	var problems []string

	if !v.PersistentVolumes {
		problems = append(problems, "persistentVolumes needs the gateway setting persistentVolumes.enabled")
	}

	if spec.Remote != nil {
		problems = append(problems, "persistentVolumes cannot be set together with remote")
	}

	if spec.Namespace == NamespaceShared {
		problems = append(problems, "persistentVolumes needs namespace: per-instance")
	}

	if spec.Scope == ScopePerSession {
		problems = append(problems, "a per-session template cannot have persistentVolumes")
	}

	effective := templatescope.Effective(spec)

	for _, volume := range spec.PersistentVolumes {
		problems = append(problems, volumeScopeProblem(spec, volume, effective)...)

		if quantity, err := resource.ParseQuantity(volume.Size); err != nil || quantity.Sign() <= 0 {
			problems = append(problems, fmt.Sprintf("persistentVolumes `%s`: size must be a quantity greater than 0", volume.Name))
		}
	}

	problems = append(problems, validateVolumeResources(spec)...)

	return problems
}

// volumeScopeProblem checks a volume's scope against the template's effective
// scope.
func volumeScopeProblem(spec v1alpha1.MCPServerTemplateSpec, volume v1alpha1.PersistentVolume, effective string) []string {
	if volume.Scope == "" || volume.Scope == templatescope.VolumeScope(effective) {
		return nil
	}

	switch {
	case effective == ScopePerSession && spec.Scope == ScopePerSession:
		// The per-session rule already names the problem.
		return nil
	case effective == ScopeShared && volume.Scope == templatescope.VolumePerUser:
		return []string{fmt.Sprintf("persistentVolumes `%s`: scope `%s` needs a user, and a shared template has none", volume.Name, volume.Scope)}
	case volume.Scope == templatescope.VolumePerTemplate || volume.Scope == templatescope.VolumePerUser:
		return []string{fmt.Sprintf("persistentVolumes `%s`: scope `%s` is wider than the template's effective scope `%s`", volume.Name, volume.Scope, effective)}
	default:
		return []string{fmt.Sprintf("persistentVolumes `%s`: scope `%s` must be one of per-template, per-user", volume.Name, volume.Scope)}
	}
}

// validateVolumeResources checks that no resource is a bare Pod, which
// toolgate can't scale to zero before teardown, and that no resource is a
// PersistentVolumeClaim that a volume already makes.
func validateVolumeResources(spec v1alpha1.MCPServerTemplateSpec) []string {
	var problems []string

	names := make(map[string]bool, len(spec.PersistentVolumes))
	for _, volume := range spec.PersistentVolumes {
		names[volume.Name] = true
	}

	podReported := false

	for index, raw := range spec.Resources {
		var object struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}

		if err := json.Unmarshal(raw.Raw, &object); err != nil {
			continue
		}

		if object.APIVersion == "v1" && object.Kind == "Pod" && !podReported {
			podReported = true

			problems = append(problems, "persistentVolumes cannot be used with a Pod in resources. Use a Deployment or StatefulSet.")
		}

		if object.APIVersion == "v1" && object.Kind == "PersistentVolumeClaim" && names[object.Metadata.Name] {
			problems = append(problems, fmt.Sprintf("resources[%d] is a PersistentVolumeClaim named `%s`, which persistentVolumes already makes", index, object.Metadata.Name))
		}
	}

	return problems
}
