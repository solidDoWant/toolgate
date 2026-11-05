// Package catalog reads MCPServerTemplate objects, validates them beyond the
// CEL rules in their CRD, reports the result in their status, and serves the
// valid templates to the rest of the gateway (SPEC.md §8 and §9).
package catalog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// Scope and namespace values of a template.
const (
	ScopePerSession = "per-session"
	ScopePerUser    = "per-user"
	ScopeShared     = "shared"

	NamespacePerInstance = "per-instance"
	NamespaceShared      = "shared"
)

// maxReasonLength bounds the reason written to a template's status and log.
const maxReasonLength = 1024

// serverNamePattern is the pattern every server (template) name must match.
var serverNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// allowedKinds are the kinds a phase 1 template may contain (SPEC.md §9).
var allowedKinds = map[schema.GroupKind]bool{
	{Group: "", Kind: "Pod"}:                                        true,
	{Group: "apps", Kind: "Deployment"}:                             true,
	{Group: "apps", Kind: "StatefulSet"}:                            true,
	{Group: "", Kind: "Service"}:                                    true,
	{Group: "", Kind: "ConfigMap"}:                                  true,
	{Group: "", Kind: "Secret"}:                                     true,
	{Group: "", Kind: "ServiceAccount"}:                             true,
	{Group: "networking.k8s.io", Kind: "NetworkPolicy"}:             true,
	{Group: "", Kind: "PersistentVolumeClaim"}:                      true,
	{Group: CiliumNetworkPolicy.Group, Kind: "CiliumNetworkPolicy"}: true,
}

// CiliumNetworkPolicy is the kind allowed only when its CRD exists in the
// cluster.
var CiliumNetworkPolicy = schema.GroupKind{Group: "cilium.io", Kind: "CiliumNetworkPolicy"}

// rbacGroup is the API group of RBAC objects, which templates may never hold.
const rbacGroup = "rbac.authorization.k8s.io"

// Validator checks templates beyond the CRD's CEL rules.
type Validator struct {
	// CiliumNetworkPolicy allows CiliumNetworkPolicy objects. Set it when
	// the CiliumNetworkPolicy CRD exists in the cluster.
	CiliumNetworkPolicy bool
	// SharedNamespace is the gateway's shared namespace. When it is empty,
	// every `namespace: shared` template is invalid.
	SharedNamespace string
}

// Validate returns every problem found in template, or nil when it is valid.
func (v Validator) Validate(template *v1alpha1.MCPServerTemplate) []string {
	var problems []string

	if !serverNamePattern.MatchString(template.Name) {
		problems = append(problems, fmt.Sprintf("the name %q is the server name and must match %s", template.Name, serverNamePattern))
	}

	spec := template.Spec

	if !slices.Contains([]string{ScopePerSession, ScopePerUser, ScopeShared}, spec.Scope) {
		problems = append(problems, fmt.Sprintf("scope %q must be one of per-session, per-user, shared", spec.Scope))
	}

	switch spec.Namespace {
	case "", NamespacePerInstance:
	case NamespaceShared:
		if spec.Scope != ScopeShared {
			problems = append(problems, "namespace: shared is allowed only with scope: shared")
		}

		if v.SharedNamespace == "" {
			problems = append(problems, "The gateway has no shared namespace configured (`config.sharedNamespace.name`).")
		}

		problems = append(problems, validateSharedNames(template)...)
	default:
		problems = append(problems, fmt.Sprintf("namespace %q must be one of per-instance, shared", spec.Namespace))
	}

	for index, raw := range spec.Resources {
		problems = append(problems, v.validateResource(spec.Scope, index, raw.Raw)...)
	}

	return problems
}

// Reason joins problems into one status and log message of bounded length.
func Reason(problems []string) string {
	reason := strings.Join(problems, "; ")
	if len(reason) > maxReasonLength {
		reason = strings.ToValidUTF8(reason[:maxReasonLength-3], "") + "..."
	}

	return reason
}

func (v Validator) validateResource(scope string, index int, raw []byte) []string {
	where := "resources[" + strconv.Itoa(index) + "]"

	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return []string{where + " must be a Kubernetes object"}
	}

	apiVersion, _ := object["apiVersion"].(string)
	kind, _ := object["kind"].(string)

	if apiVersion == "" || kind == "" {
		return []string{where + " must have apiVersion and kind"}
	}

	groupVersion, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return []string{fmt.Sprintf("%s: apiVersion %q is not valid", where, apiVersion)}
	}

	groupKind := schema.GroupKind{Group: groupVersion.Group, Kind: kind}

	var problems []string

	switch {
	case groupKind.Group == rbacGroup:
		problems = append(problems, fmt.Sprintf("%s: %s is an RBAC object, which templates may not contain", where, describeKind(apiVersion, kind)))
	case groupKind == CiliumNetworkPolicy && !v.CiliumNetworkPolicy:
		problems = append(problems, fmt.Sprintf("%s: %s is allowed only when the CiliumNetworkPolicy CRD exists in the cluster", where, describeKind(apiVersion, kind)))
	case !allowedKinds[groupKind]:
		problems = append(problems, fmt.Sprintf("%s: %s is not an allowed kind; templates may contain only the namespaced kinds Pod, Deployment, StatefulSet, Service, ConfigMap, Secret, ServiceAccount, NetworkPolicy, PersistentVolumeClaim and CiliumNetworkPolicy", where, describeKind(apiVersion, kind)))
	}

	isSecret := groupKind == schema.GroupKind{Group: "", Kind: "Secret"}

	var variableProblems []string

	walkStrings(object, []string{where}, func(path []string, text string, isKey bool) {
		for _, variable := range variables(text) {
			location := strings.Join(path, "")
			if isKey {
				location += " (a key)"
			}

			inSecretData := isSecret && !isKey && isSecretDataValue(path)
			if isSecretVariable(variable) && !inSecretData {
				variableProblems = append(variableProblems, fmt.Sprintf("%s: ${%s} is allowed only in the data or stringData values of a Secret", location, variable))
			}

			if scope == ScopeShared && isPerUserVariable(variable) {
				variableProblems = append(variableProblems, fmt.Sprintf("%s: a shared template may not use ${%s}", location, variable))
			}
		}
	})

	// Map order is random; sort so the reason is stable between reads.
	slices.Sort(variableProblems)

	return append(problems, variableProblems...)
}

// instanceIDVariable is the variable that tells instances apart in the shared
// namespace.
const instanceIDVariable = "${instance.id}"

// instanceLabel is the label every rendered object carries with its
// instance's ID.
const instanceLabel = "toolgate.soliddowant.dev/instance"

// validateSharedNames checks that a `namespace: shared` template tells its
// instances apart by itself, because toolgate never renames objects
// (docs/design/phase-1.md §7): every name and the endpoint Service contain
// ${instance.id}, and every selector of a known kind selects the instance
// label.
func validateSharedNames(template *v1alpha1.MCPServerTemplate) []string {
	var problems []string

	for _, raw := range template.Spec.Resources {
		var object map[string]any
		if err := json.Unmarshal(raw.Raw, &object); err != nil || object == nil {
			continue
		}

		apiVersion, _ := object["apiVersion"].(string)
		kind, _ := object["kind"].(string)
		name, _ := nestedString(object, "metadata", "name")
		where := "`" + kind + "/" + name + "`"

		groupVersion, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			continue
		}

		groupKind := schema.GroupKind{Group: groupVersion.Group, Kind: kind}

		if groupKind == CiliumNetworkPolicy {
			problems = append(problems, where+": a CiliumNetworkPolicy is not allowed in a shared namespace, because toolgate can't check its selectors.")

			continue
		}

		if !strings.Contains(name, instanceIDVariable) {
			problems = append(problems, where+": `metadata.name` must contain `"+instanceIDVariable+"` in a shared namespace.")
		}

		var labelMaps [][]string

		switch groupKind {
		case schema.GroupKind{Group: "", Kind: "Service"}:
			spec, _ := object["spec"].(map[string]any)
			if _, found := spec["selector"]; found {
				labelMaps = [][]string{{"spec", "selector"}}
			}
		case schema.GroupKind{Group: "apps", Kind: "Deployment"}, schema.GroupKind{Group: "apps", Kind: "StatefulSet"}:
			labelMaps = [][]string{{"spec", "selector", "matchLabels"}, {"spec", "template", "metadata", "labels"}}
		case schema.GroupKind{Group: "networking.k8s.io", Kind: "NetworkPolicy"}:
			labelMaps = [][]string{{"spec", "podSelector", "matchLabels"}}
		}

		for _, fields := range labelMaps {
			if value, _ := nestedString(object, append(fields, instanceLabel)...); value != instanceIDVariable {
				problems = append(problems, fmt.Sprintf("%s: `%s` must contain `%s: %s` in a shared namespace.",
					where, strings.Join(fields, "."), instanceLabel, instanceIDVariable))
			}
		}
	}

	if !strings.Contains(template.Spec.Endpoint.Service, instanceIDVariable) {
		problems = append(problems, "`endpoint.service` must contain `"+instanceIDVariable+"` in a shared namespace.")
	}

	return problems
}

// nestedString returns the string at fields in object, if there is one.
func nestedString(object map[string]any, fields ...string) (string, bool) {
	var node any = object

	for _, field := range fields {
		parent, isMap := node.(map[string]any)
		if !isMap {
			return "", false
		}

		node = parent[field]
	}

	text, isString := node.(string)

	return text, isString
}

func describeKind(apiVersion, kind string) string {
	return apiVersion + " " + kind
}

// isSecretDataValue reports whether path (rooted at the resource) is a value
// directly under the object's data or stringData.
func isSecretDataValue(path []string) bool {
	return len(path) == 3 && (path[1] == ".data" || path[1] == ".stringData")
}

func isSecretVariable(variable string) bool {
	return strings.HasPrefix(variable, "secret:") || strings.HasPrefix(variable, "resolver:")
}

// isPerUserVariable reports whether variable depends on a principal, a
// workspace, a session or a user's secret, none of which a shared instance
// has.
func isPerUserVariable(variable string) bool {
	if isSecretVariable(variable) {
		return true
	}

	for _, prefix := range []string{"principal.", "workspace.", "session."} {
		if strings.HasPrefix(variable, prefix) {
			return true
		}
	}

	return false
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

// walkStrings calls visit for every string scalar and map key in node, with
// the path to it.
func walkStrings(node any, path []string, visit func(path []string, text string, isKey bool)) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			childPath := append(slices.Clip(path), "."+key)
			visit(childPath, key, true)
			walkStrings(child, childPath, visit)
		}
	case []any:
		for index, child := range value {
			walkStrings(child, append(slices.Clip(path), "["+strconv.Itoa(index)+"]"), visit)
		}
	case string:
		visit(path, value, false)
	}
}

// SecretNames returns the names of the user secrets template's resources
// use through `${secret:<name>}`, sorted and without duplicates.
func SecretNames(template *v1alpha1.MCPServerTemplate) []string {
	var names []string

	for _, raw := range template.Spec.Resources {
		var object any
		if err := json.Unmarshal(raw.Raw, &object); err != nil {
			continue
		}

		walkStrings(object, nil, func(_ []string, text string, _ bool) {
			for _, variable := range variables(text) {
				if name, found := strings.CutPrefix(variable, "secret:"); found {
					names = append(names, name)
				}
			}
		})
	}

	slices.Sort(names)

	return slices.Compact(names)
}
