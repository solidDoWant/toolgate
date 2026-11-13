// Package templatescope decides a template's effective scope from the
// variables it references. A template's scope must be at least as narrow as
// its variables: a principal, a user's secret or a resolver needs per-user,
// and a session or a workspace needs per-session. When the template states no
// scope, the effective scope is the broadest one its variables allow. The
// catalog validator, the renderer and every view of a template use this one
// package, so they can't disagree.
package templatescope

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// The scopes, from broadest to narrowest.
const (
	Shared     = "shared"
	PerUser    = "per-user"
	PerSession = "per-session"
)

// rank orders the scopes from broadest (0) to narrowest.
func rank(scope string) int {
	switch scope {
	case PerUser:
		return 1
	case PerSession:
		return 2
	default:
		return 0
	}
}

// Valid reports whether scope is one of the three scopes.
func Valid(scope string) bool {
	return scope == Shared || scope == PerUser || scope == PerSession
}

// ForVariable returns the broadest scope in which variable is defined.
func ForVariable(variable string) string {
	switch {
	case strings.HasPrefix(variable, "session."), strings.HasPrefix(variable, "workspace."):
		return PerSession
	case strings.HasPrefix(variable, "principal."), strings.HasPrefix(variable, "secret:"), strings.HasPrefix(variable, "resolver:"):
		return PerUser
	default:
		return Shared
	}
}

// Allows reports whether a template of scope may use a variable that needs
// needed: scope must be needed or narrower.
func Allows(scope, needed string) bool {
	return rank(scope) >= rank(needed)
}

// Display returns how a variable is shown in a reason. Secret and resolver
// names are left out, so the reason shows only the kind of variable.
func Display(variable string) string {
	switch {
	case strings.HasPrefix(variable, "secret:"):
		return "${secret:…}"
	case strings.HasPrefix(variable, "resolver:"):
		return "${resolver:…}"
	default:
		return "${" + variable + "}"
	}
}

// Decision is a template's effective scope and why.
type Decision struct {
	// Scope is the effective scope: the stated one when there is one,
	// otherwise the broadest one the variables allow.
	Scope string
	// Reason says why, for example "per-user: `remote.headers.Authorization`
	// uses `${secret:…}`".
	Reason string
	// Problem is set when the stated scope is broader than a variable allows.
	Problem string
}

// reference is one variable a template uses and where.
type reference struct {
	location string
	variable string
}

// Decide returns the effective scope of spec. A stated scope that isn't one
// of the three is returned as it is, for the validator to refuse.
func Decide(spec v1alpha1.MCPServerTemplateSpec) Decision {
	narrowest, found := narrowestReference(spec)

	derived := Shared
	because := "references nothing per-user"

	if found {
		derived = ForVariable(narrowest.variable)
		because = "`" + narrowest.location + "` uses `" + Display(narrowest.variable) + "`"
	}

	if spec.Scope == "" {
		return Decision{Scope: derived, Reason: derived + ": " + because}
	}

	if !Valid(spec.Scope) {
		return Decision{Scope: spec.Scope}
	}

	if !Allows(spec.Scope, derived) {
		return Decision{Scope: spec.Scope, Problem: fmt.Sprintf("`scope: %s`, but `%s` uses `%s`, which needs `%s` or narrower",
			spec.Scope, narrowest.location, Display(narrowest.variable), derived)}
	}

	if spec.Scope == derived {
		return Decision{Scope: spec.Scope, Reason: spec.Scope + ": " + because}
	}

	return Decision{Scope: spec.Scope, Reason: spec.Scope + ": set by `scope`"}
}

// Effective returns the effective scope of spec.
func Effective(spec v1alpha1.MCPServerTemplateSpec) string {
	if spec.Scope != "" {
		return spec.Scope
	}

	return Decide(spec).Scope
}

// narrowestReference returns the per-identity reference that needs the
// narrowest scope, or false when there is none.
// Among equals it returns the first by location, so the reason is stable
// between reads.
func narrowestReference(spec v1alpha1.MCPServerTemplateSpec) (reference, bool) {
	var references []reference

	add := func(location, text string) {
		for _, variable := range variables(text) {
			if ForVariable(variable) != Shared {
				references = append(references, reference{location: location, variable: variable})
			}
		}
	}

	for index, raw := range spec.Resources {
		var object any
		if err := json.Unmarshal(raw.Raw, &object); err != nil {
			continue
		}

		walkStrings(object, "resources["+strconv.Itoa(index)+"]", add)
	}

	add("endpoint.service", spec.Endpoint.Service)

	if spec.HealthCheck != nil {
		add("healthCheck.http.service", spec.HealthCheck.HTTP.Service)
	}

	if spec.Remote != nil {
		for name, value := range spec.Remote.Headers {
			add("remote.headers."+name, value)
		}
	}

	if len(references) == 0 {
		return reference{}, false
	}

	return slices.MinFunc(references, func(a, b reference) int {
		if order := rank(ForVariable(b.variable)) - rank(ForVariable(a.variable)); order != 0 {
			return order
		}

		if order := strings.Compare(a.location, b.location); order != 0 {
			return order
		}

		return strings.Compare(a.variable, b.variable)
	}), true
}

// walkStrings calls visit for every string value and map key in node, with
// the path to it.
func walkStrings(node any, path string, visit func(location, text string)) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			childPath := path + "." + key
			visit(childPath, key)
			walkStrings(child, childPath, visit)
		}
	case []any:
		for index, child := range value {
			walkStrings(child, path+"["+strconv.Itoa(index)+"]", visit)
		}
	case string:
		visit(path, value)
	}
}

// variables returns the names of the ${…} variables in text. `$${` is an
// escaped literal `${` and starts no variable.
func variables(text string) []string {
	var names []string

	for index := 0; index < len(text); {
		rest := text[index:]

		switch {
		case strings.HasPrefix(rest, "$${"):
			index += 3
		case strings.HasPrefix(rest, "${"):
			end := strings.IndexByte(rest[2:], '}')
			if end < 0 {
				return names
			}

			names = append(names, rest[2:2+end])
			index += 2 + end + 1
		default:
			index++
		}
	}

	return names
}

// Volume scopes of a template's persistentVolumes.
const (
	VolumePerTemplate = "per-template"
	VolumePerUser     = "per-user"
)

// VolumeScope returns the scope a persistent volume of a template with
// effective scope templateScope may have: per-template for a shared template,
// per-user for a per-user one, and "" for a per-session one, which can have
// none.
func VolumeScope(templateScope string) string {
	switch templateScope {
	case Shared:
		return VolumePerTemplate
	case PerUser:
		return VolumePerUser
	default:
		return ""
	}
}

// EffectiveVolumeScope returns the scope of volume in a template of spec: the
// volume's own scope when it sets one, otherwise the one the template's
// effective scope allows.
func EffectiveVolumeScope(spec v1alpha1.MCPServerTemplateSpec, volume v1alpha1.PersistentVolume) string {
	if volume.Scope != "" {
		return volume.Scope
	}

	return VolumeScope(Effective(spec))
}
