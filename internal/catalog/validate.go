// Package catalog reads MCPServerTemplate objects, validates them beyond the
// CEL rules in their CRD, reports the result in their status, and serves the
// valid templates to the rest of the gateway.
package catalog

import (
	"github.com/gobwas/glob"

	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/templatescope"
)

// Scope and namespace values of a template.
const (
	ScopePerSession = templatescope.PerSession
	ScopePerUser    = templatescope.PerUser
	ScopeShared     = templatescope.Shared

	NamespacePerInstance = "per-instance"
	NamespaceShared      = "shared"
)

// maxReasonLength bounds the reason written to a template's status and log.
const maxReasonLength = 1024

// serverNamePattern is the pattern every server (template) name must match.
var serverNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// sharedNamespaceKinds are the kinds whose names and selectors toolgate can
// check, the only kinds a `namespace: shared` template may contain.
var sharedNamespaceKinds = map[schema.GroupKind]bool{
	{Group: "", Kind: "Pod"}:                            true,
	{Group: "apps", Kind: "Deployment"}:                 true,
	{Group: "apps", Kind: "StatefulSet"}:                true,
	{Group: "", Kind: "Service"}:                        true,
	{Group: "", Kind: "ConfigMap"}:                      true,
	{Group: "", Kind: "Secret"}:                         true,
	{Group: "", Kind: "ServiceAccount"}:                 true,
	{Group: "networking.k8s.io", Kind: "NetworkPolicy"}: true,
	{Group: "", Kind: "PersistentVolumeClaim"}:          true,
}

// Validator checks templates beyond the CRD's CEL rules.
type Validator struct {
	// Mapper maps a template's kinds to their REST mappings. A kind it
	// can't map, or maps to a cluster-scoped resource, is invalid, because
	// teardown deletes only the instance namespace. When it is nil, every
	// kind is invalid.
	Mapper meta.RESTMapper
	// SharedNamespace is the gateway's shared namespace. When it is empty,
	// every `namespace: shared` template is invalid.
	SharedNamespace string
	// Resolvers are the names of the configured resolvers. A template that
	// names any other resolver is invalid.
	Resolvers []string
	// Exchanges are the names of the configured token exchanges. A template
	// that names any other exchange is invalid.
	Exchanges []string
	// TemplateSecretsNamespace is the gateway's template secrets namespace.
	// When it is empty, every template that uses `${templateSecret:…}` is
	// invalid.
	TemplateSecretsNamespace string
}

// Validate returns every problem found in template, or nil when it is valid.
func (v Validator) Validate(template *v1alpha1.MCPServerTemplate) []string {
	var problems []string

	if !serverNamePattern.MatchString(template.Name) {
		problems = append(problems, fmt.Sprintf("the name %q is the server name and must match %s", template.Name, serverNamePattern))
	}

	problems = append(problems, validateReservedName(template)...)
	problems = append(problems, validateTools(template)...)

	spec := template.Spec

	scope := templatescope.Decide(spec)
	remoteProblems := validateRemote(spec.Remote)

	switch {
	case spec.Scope != "" && !templatescope.Valid(spec.Scope):
		problems = append(problems, fmt.Sprintf("scope %q must be one of per-session, per-user, shared", spec.Scope))
	case scope.Problem != "" && len(remoteProblems) == 0:
		// A header that uses a variable headers don't allow is already
		// reported, and no scope makes it valid.
		problems = append(problems, scope.Problem)
	}

	switch spec.Namespace {
	case "", NamespacePerInstance:
	case NamespaceShared:
		if scope.Scope != ScopeShared {
			problems = append(problems, "namespace: shared is allowed only with scope: shared, and this template's scope is "+scope.Scope)
		}

		if v.SharedNamespace == "" {
			problems = append(problems, "The gateway has no shared namespace configured (`config.sharedNamespace.name`).")
		}

		problems = append(problems, validateSharedNames(template)...)
	default:
		problems = append(problems, fmt.Sprintf("namespace %q must be one of per-instance, shared", spec.Namespace))
	}

	for _, list := range []struct {
		field    string
		patterns []string
	}{
		{"users", spec.Access.Users},
		{"groups", spec.Access.Groups},
		{"spiffeIDs", spec.Access.SPIFFEIDs},
	} {
		for index, pattern := range list.patterns {
			if _, err := glob.Compile(pattern); err != nil {
				problems = append(problems, fmt.Sprintf("access.%s[%d]: %q is not a valid pattern", list.field, index, pattern))
			}
		}
	}

	for index, raw := range spec.Resources {
		problems = append(problems, v.validateResource(index, raw.Raw)...)
	}

	problems = append(problems, validateHealthCheck(spec)...)
	problems = append(problems, validateUpdatePolicy(spec)...)
	problems = append(problems, remoteProblems...)

	return problems
}

// minUpdateIdleFor is the shortest idleFor of the whenIdle update policy: a
// running call keeps its instance's last use at most a minute old.
const minUpdateIdleFor = time.Minute

// validateUpdatePolicy checks the template's updatePolicy, if any.
func validateUpdatePolicy(spec v1alpha1.MCPServerTemplateSpec) []string {
	policy := spec.UpdatePolicy
	if policy == nil {
		return nil
	}

	switch policy.Mode {
	case v1alpha1.UpdateModeManual, v1alpha1.UpdateModeImmediate:
		if policy.IdleFor != nil {
			return []string{"updatePolicy.idleFor is allowed only with mode whenIdle"}
		}
	case v1alpha1.UpdateModeWhenIdle:
		if policy.IdleFor == nil || policy.IdleFor.Duration < minUpdateIdleFor {
			return []string{"updatePolicy.idleFor must be at least " + minUpdateIdleFor.String() + " with mode whenIdle"}
		}
	default:
		return []string{fmt.Sprintf("updatePolicy.mode %q must be one of manual, immediate, whenIdle", policy.Mode)}
	}

	return nil
}

// Health check bounds. The CRD's CEL rules check
// the same ones.
const (
	minHealthInterval = 10 * time.Second
	maxHealthInterval = 10 * time.Minute
	minHealthTimeout  = time.Second
	maxHealthTimeout  = time.Minute
	maxHealthPath     = 1024
	maxServiceName    = 63
)

// validateHealthCheck checks the template's healthCheck, if any. Zero values
// are the CRD defaults, which are in bounds.
func validateHealthCheck(spec v1alpha1.MCPServerTemplateSpec) []string {
	check := spec.HealthCheck
	if check == nil {
		return nil
	}

	var problems []string

	if check.HTTP.Service == "" || len(check.HTTP.Service) > maxServiceName {
		problems = append(problems, "`healthCheck.http.service` must be 1 to 63 characters.")
	}

	if spec.Namespace == NamespaceShared && !strings.Contains(check.HTTP.Service, instanceIDVariable) {
		problems = append(problems, "`healthCheck.http.service` must contain `"+instanceIDVariable+"` in a shared namespace.")
	}

	if check.HTTP.Port < 1 || check.HTTP.Port > 65535 {
		problems = append(problems, "`healthCheck.http.port` must be 1 to 65535.")
	}

	if !strings.HasPrefix(check.HTTP.Path, "/") || len(check.HTTP.Path) > maxHealthPath {
		problems = append(problems, "`healthCheck.http.path` must start with / and be at most 1024 characters.")
	}

	if status := check.HTTP.ExpectedStatus; status != 0 && (status < 100 || status > 599) {
		problems = append(problems, "`healthCheck.http.expectedStatus` must be 100 to 599.")
	}

	interval, timeout := check.Interval.Duration, check.Timeout.Duration
	if interval == 0 {
		interval = 30 * time.Second
	}

	if timeout == 0 {
		timeout = 5 * time.Second
	}

	if interval < minHealthInterval || interval > maxHealthInterval {
		problems = append(problems, "`healthCheck.interval` must be 10s to 10m.")
	}

	if timeout < minHealthTimeout || timeout > maxHealthTimeout {
		problems = append(problems, "`healthCheck.timeout` must be 1s to 60s.")
	}

	if timeout >= interval {
		problems = append(problems, "`healthCheck.timeout` must be less than `healthCheck.interval`.")
	}

	for name, value := range map[string]int32{"failureThreshold": check.FailureThreshold, "successThreshold": check.SuccessThreshold, "maxRepairs": check.MaxRepairs} {
		if value != 0 && (value < 1 || value > 10) {
			problems = append(problems, "`healthCheck."+name+"` must be 1 to 10.")
		}
	}

	if !slices.Contains([]string{"", "replace", "reapply", "none"}, check.OnFailure) {
		problems = append(problems, fmt.Sprintf("`healthCheck.onFailure` %q must be one of replace, reapply, none.", check.OnFailure))
	}

	slices.Sort(problems)

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

func (v Validator) validateResource(index int, raw []byte) []string {
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

	if problem := v.scopeProblem(groupKind, groupVersion.Version); problem != "" {
		problems = append(problems, fmt.Sprintf("%s: %s %s", where, describeKind(apiVersion, kind), problem))
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

			if name, found := strings.CutPrefix(variable, "resolver:"); found && !slices.Contains(v.Resolvers, name) {
				variableProblems = append(variableProblems, fmt.Sprintf("%s: resolver `%s` isn't configured", location, name))
			}

			if inSecretData {
				variableProblems = append(variableProblems, v.credentialVariableProblems(location, text, variable)...)
			}
		}
	})

	// Map order is random; sort so the reason is stable between reads.
	slices.Sort(variableProblems)

	return append(problems, variableProblems...)
}

// scopeProblem returns why a template may not contain groupKind at version,
// or "" when it is a namespaced kind the cluster serves.
func (v Validator) scopeProblem(groupKind schema.GroupKind, version string) string {
	if v.Mapper == nil {
		return "is not installed in the cluster"
	}

	mapping, err := v.Mapper.RESTMapping(groupKind, version)
	if err != nil {
		return "is not installed in the cluster; a CRD installed after the gateway started needs a gateway restart"
	}

	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		return "is cluster-scoped; templates may contain only namespaced kinds"
	}

	return ""
}

// instanceIDVariable is the variable that tells instances apart in the shared
// namespace.
const instanceIDVariable = "${instance.id}"

// instanceLabel is the label every rendered object carries with its
// instance's ID.
const instanceLabel = "toolgate.soliddowant.dev/instance"

// validateSharedNames checks that a `namespace: shared` template tells its
// instances apart by itself, because toolgate never renames objects:
// every name and the endpoint Service contain
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

		if !sharedNamespaceKinds[groupKind] {
			problems = append(problems, where+": a "+kind+" is not allowed in a shared namespace, because toolgate can't check its names and selectors.")

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
	return strings.HasPrefix(variable, "secret:") || strings.HasPrefix(variable, "resolver:") ||
		strings.HasPrefix(variable, exchangePrefix) || strings.HasPrefix(variable, templateSecretPrefix)
}

const (
	exchangePrefix       = "exchange:"
	templateSecretPrefix = "templateSecret:"
)

// secretKeyPattern is the pattern of a key in a Secret's data.
var secretKeyPattern = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)

// credentialVariableProblems checks one `${exchange:…}` or
// `${templateSecret:…}` variable in a Secret value: it must be the whole
// value, and it must name something the gateway has. It never reads a Secret.
func (v Validator) credentialVariableProblems(location, text, variable string) []string {
	var problems []string

	argument, isExchange := strings.CutPrefix(variable, exchangePrefix)

	if !isExchange {
		if _, isTemplateSecret := strings.CutPrefix(variable, templateSecretPrefix); !isTemplateSecret {
			return nil
		}
	}

	if text != "${"+variable+"}" {
		problems = append(problems, fmt.Sprintf("%s: ${%s} must be the whole value, with no other text", location, variable))
	}

	if isExchange {
		if !slices.Contains(v.Exchanges, argument) {
			problems = append(problems, fmt.Sprintf("%s: token exchange `%s` isn't configured", location, argument))
		}

		return problems
	}

	reference := strings.TrimPrefix(variable, templateSecretPrefix)

	if v.TemplateSecretsNamespace == "" {
		problems = append(problems, location+": ${templateSecret:…} needs a template secrets namespace, and the gateway has none (`config.templateSecretsNamespace`)")
	}

	if _, _, valid := ParseTemplateSecret(reference); !valid {
		problems = append(problems, location+": ${templateSecret:…} must be NAME/KEY, with a Secret name and a key")
	}

	return problems
}

// ParseTemplateSecret splits the argument of `${templateSecret:NAME/KEY}` at
// the first `/`. valid is false when NAME is not a DNS-1123 subdomain or KEY
// is not a valid Secret key.
func ParseTemplateSecret(reference string) (name, key string, valid bool) {
	name, key, found := strings.Cut(reference, "/")
	if !found || len(validation.IsDNS1123Subdomain(name)) > 0 || !secretKeyPattern.MatchString(key) {
		return name, key, false
	}

	return name, key, true
}

// UsesExchange reports whether any `${exchange:…}` variable appears in the
// values of the template's resources.
func UsesExchange(template *v1alpha1.MCPServerTemplate) bool {
	found := false

	for _, raw := range template.Spec.Resources {
		var object any
		if err := json.Unmarshal(raw.Raw, &object); err != nil {
			continue
		}

		walkStrings(object, nil, func(_ []string, text string, isKey bool) {
			if isKey {
				return
			}

			for _, variable := range variables(text) {
				if strings.HasPrefix(variable, exchangePrefix) {
					found = true
				}
			}
		})
	}

	return found
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

// RequiredSecretNames returns the names of the user secrets the template
// needs: the ones its resources use and the ones its remote headers use,
// sorted and without duplicates.
func RequiredSecretNames(template *v1alpha1.MCPServerTemplate) []string {
	names := SecretNames(template)

	if template.Spec.Remote != nil {
		for _, value := range template.Spec.Remote.Headers {
			for _, variable := range variables(value) {
				if name, found := strings.CutPrefix(variable, "secret:"); found {
					names = append(names, name)
				}
			}
		}
	}

	slices.Sort(names)

	return slices.Compact(names)
}
