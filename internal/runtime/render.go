// Package runtime renders MCPServerTemplate resources into the Kubernetes
// objects of one instance.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// Labels every rendered object carries.
const (
	LabelInstance  = "toolgate.soliddowant.dev/instance"
	LabelTemplate  = "toolgate.soliddowant.dev/template"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelOwner is the hashed owner principal of an instance namespace.
	LabelOwner = "toolgate.soliddowant.dev/owner"

	// ManagedByValue is the value of LabelManagedBy.
	ManagedByValue = "toolgate"

	// LabelWritable marks a namespace the admission policy lets the gateway
	// write in. It isn't LabelManagedBy because Helm 4 overwrites that label
	// on every object it installs, the chart's shared namespace included.
	LabelWritable = "toolgate.soliddowant.dev/writable"
)

// scopeShared is the scope of a template with one instance for everyone.
const scopeShared = "shared"

// claimedHashLength is the number of hex characters a claimed value is hashed
// to in a name or label. It fits a DNS label and a label value (63
// characters) with room for a prefix.
const claimedHashLength = 32

// Identity is the identity snapshot an instance is rendered with.
type Identity struct {
	PrincipalID string
	// PrincipalEmail is the verified email, or empty when there is none.
	PrincipalEmail string
	PrincipalName  string
	// PrincipalGroups are the principal's groups, sent to resolvers.
	PrincipalGroups []string
	WorkspaceID     string
	// Kind is the identity's kind (auth.KindUser, auth.KindServiceAccount
	// or auth.KindSPIFFE), as recorded on the session.
	Kind string
	// Attested is true when the workspace was verified by the platform.
	// Otherwise WorkspaceID is a claimed value.
	Attested  bool
	SessionID string
}

// Instance names the instance being rendered.
type Instance struct {
	ID        string
	Namespace string
}

// SecretSource resolves ${secret:…} for the identity being rendered. The
// secrets package implements it.
type SecretSource interface {
	// Secret returns the value of the user's secret name and the time it was
	// last set.
	Secret(ctx context.Context, name string) (string, time.Time, error)
}

// Input is everything one render needs.
type Input struct {
	Template *v1alpha1.MCPServerTemplate
	Identity Identity
	Instance Instance
	Secrets  SecretSource
	// Resolvers calls the resolvers ${resolver:…} names. Nil means none are
	// configured.
	Resolvers ResolverCaller
	// Metadata is put on every rendered object, and on the pod template of
	// the kinds that have one. Its keys win over the template's own.
	Metadata InstanceMetadata
}

// Result is a rendered instance.
type Result struct {
	// Objects are the rendered objects, in template order.
	Objects []*unstructured.Unstructured
	// SecretsUsed are the names of the user secrets the render used, sorted.
	SecretsUsed []string
	// SecretVersions maps each used secret's name to the time it was last
	// set, as read by the render.
	SecretVersions map[string]time.Time
	// ResolvedExpiresAt is the earliest expiresAt of the resolver values
	// the render used, or zero when it used none.
	ResolvedExpiresAt time.Time
	// Metadata is the labels and annotations the objects carry, for the
	// objects toolgate adds around them.
	Metadata InstanceMetadata
}

// VariableError is a variable that could not be rendered. Its message names
// the variable and never a value.
type VariableError struct {
	// Path locates the string the variable is in.
	Path string
	// Variable is the variable's name, without ${ and }.
	Variable string
	// Reason says why it could not be rendered.
	Reason string
	// Err is the underlying error, if any.
	Err error
}

func (e *VariableError) Error() string {
	message := fmt.Sprintf("%s: ${%s} %s", e.Path, e.Variable, e.Reason)
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}

	return message
}

func (e *VariableError) Unwrap() error { return e.Err }

// Render renders every resource of the input's template for one instance.
// Variables are substituted only inside string values of the parsed objects,
// never in keys, and a substituted value is never scanned again, so a value
// can't add fields or variables.
func Render(ctx context.Context, input Input) (*Result, error) {
	renderer := &renderer{ctx: ctx, input: input, secretsUsed: map[string]time.Time{}}

	result := &Result{Metadata: input.Metadata}

	for index, raw := range input.Template.Spec.Resources {
		where := "resources[" + strconv.Itoa(index) + "]"

		var object map[string]any
		if err := json.Unmarshal(raw.Raw, &object); err != nil || object == nil {
			return nil, fmt.Errorf("%s must be a Kubernetes object", where)
		}

		rendered, err := renderer.node(object, []string{where})
		if err != nil {
			return nil, err
		}

		// Integers decode as float64 from JSON; go through unstructured's
		// own JSON decoding so numbers are int64 like any other object.
		encoded, err := json.Marshal(rendered)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}

		decoded := &unstructured.Unstructured{}
		if err := decoded.UnmarshalJSON(encoded); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}

		decoded.SetNamespace(input.Instance.Namespace)

		labels := mergeKeys(decoded.GetLabels(), input.Metadata.Labels)
		labels[LabelInstance] = input.Instance.ID
		labels[LabelTemplate] = input.Template.Name
		labels[LabelManagedBy] = ManagedByValue
		decoded.SetLabels(labels)

		if annotations := mergeKeys(decoded.GetAnnotations(), input.Metadata.Annotations); len(annotations) > 0 {
			decoded.SetAnnotations(annotations)
		}

		stampPodTemplate(decoded, input.Metadata)

		result.Objects = append(result.Objects, decoded)
	}

	result.SecretVersions = renderer.secretsUsed

	for name := range renderer.secretsUsed {
		result.SecretsUsed = append(result.SecretsUsed, name)
	}

	slices.Sort(result.SecretsUsed)

	for _, resolved := range renderer.resolved {
		if result.ResolvedExpiresAt.IsZero() || resolved.ExpiresAt.Before(result.ResolvedExpiresAt) {
			result.ResolvedExpiresAt = resolved.ExpiresAt
		}
	}

	return result, nil
}

// RenderEndpointService renders the template's endpoint Service name for an
// instance. A `namespace: shared` template names it with ${instance.id}, like
// the Service itself. Only instance and template variables are rendered
// there: the endpoint is never a place for an identity or a secret.
func RenderEndpointService(template *v1alpha1.MCPServerTemplate, instance Instance) (string, error) {
	shared := *template
	shared.Spec.Scope = scopeShared

	renderer := &renderer{
		ctx:         context.Background(),
		input:       Input{Template: &shared, Instance: instance, Secrets: noSecrets{}},
		secretsUsed: map[string]time.Time{},
	}

	return renderer.text(template.Spec.Endpoint.Service, []string{"endpoint", "service"})
}

// RenderHealthCheck returns the template's health check as JSON, with
// http.service rendered like the endpoint Service, or nil when the template
// has none. The path is not rendered. The result is what an instance keeps in
// its health_check column, so later template changes don't reach it.
func RenderHealthCheck(template *v1alpha1.MCPServerTemplate, instance Instance) ([]byte, error) {
	if template.Spec.HealthCheck == nil {
		return nil, nil
	}

	check := template.Spec.HealthCheck.DeepCopy()

	shared := *template
	shared.Spec.Scope = scopeShared

	renderer := &renderer{
		ctx:         context.Background(),
		input:       Input{Template: &shared, Instance: instance, Secrets: noSecrets{}},
		secretsUsed: map[string]time.Time{},
	}

	service, err := renderer.text(check.HTTP.Service, []string{"healthCheck", "http", "service"})
	if err != nil {
		return nil, err
	}

	check.HTTP.Service = service

	encoded, err := json.Marshal(check)
	if err != nil {
		return nil, fmt.Errorf("encode health check: %w", err)
	}

	return encoded, nil
}

// noSecrets is a SecretSource with no values. The shared scope set by
// RenderEndpointService refuses secret variables before they reach it.
type noSecrets struct{}

func (noSecrets) Secret(context.Context, string) (string, time.Time, error) {
	return "", time.Time{}, fmt.Errorf("no secrets here")
}

// errNoResolvers is the error of ${resolver:…} when no resolvers are
// configured.
var errNoResolvers = errors.New("no resolvers are configured")

type renderer struct {
	ctx         context.Context
	input       Input
	secretsUsed map[string]time.Time
	// resolved holds each resolver's value: each render calls each resolver
	// it names once.
	resolved map[string]Resolved
}

// node returns a copy of node with every string value rendered. path is the
// list of keys and indices leading to node, rooted at the resource.
func (r *renderer) node(node any, path []string) (any, error) {
	switch value := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))

		for key, child := range value {
			rendered, err := r.node(child, append(slices.Clip(path), key))
			if err != nil {
				return nil, err
			}

			out[key] = rendered
		}

		return out, nil
	case []any:
		out := make([]any, len(value))

		for index, child := range value {
			rendered, err := r.node(child, append(slices.Clip(path), "["+strconv.Itoa(index)+"]"))
			if err != nil {
				return nil, err
			}

			out[index] = rendered
		}

		return out, nil
	case string:
		return r.text(value, path)
	default:
		return value, nil
	}
}

// text renders the variables in one string value.
func (r *renderer) text(text string, path []string) (string, error) {
	var out strings.Builder

	for index := 0; index < len(text); {
		rest := text[index:]

		switch {
		case strings.HasPrefix(rest, "$${"):
			out.WriteString("${")

			index += 3
		case strings.HasPrefix(rest, "${"):
			end := strings.IndexByte(rest[2:], '}')
			if end < 0 {
				return "", &VariableError{Path: formatPath(path), Variable: rest[2:], Reason: "has no closing }"}
			}

			variable := rest[2 : 2+end]

			value, err := r.variable(variable, path)
			if err != nil {
				return "", err
			}

			out.WriteString(value)

			index += 2 + end + 1
		default:
			out.WriteByte(text[index])
			index++
		}
	}

	return out.String(), nil
}

// variable returns the value of one variable rendered at path.
func (r *renderer) variable(variable string, path []string) (string, error) {
	fail := func(reason string, err error) error {
		return &VariableError{Path: formatPath(path), Variable: variable, Reason: reason, Err: err}
	}

	identity := r.input.Identity
	shared := r.input.Template.Spec.Scope == scopeShared

	if shared && isPerIdentityVariable(variable) {
		return "", fail("is not defined for a shared template", nil)
	}

	switch variable {
	case "principal.id":
		return identity.PrincipalID, nil
	case "principal.email":
		if identity.PrincipalEmail == "" {
			return "", fail("is not set: the principal has no verified email", nil)
		}

		return identity.PrincipalEmail, nil
	case "principal.name":
		return identity.PrincipalName, nil
	case "workspace.id":
		if identity.Attested {
			return identity.WorkspaceID, nil
		}

		return claimed(identity.WorkspaceID, path), nil
	case "session.id":
		return claimed(identity.SessionID, path), nil
	case "instance.id":
		return r.input.Instance.ID, nil
	case "instance.namespace":
		return r.input.Instance.Namespace, nil
	case "template.name":
		return r.input.Template.Name, nil
	}

	if name, found := strings.CutPrefix(variable, "secret:"); found {
		value, setAt, err := r.input.Secrets.Secret(r.ctx, name)
		if err != nil {
			return "", fail("could not be read", err)
		}

		r.secretsUsed[name] = setAt

		return value, nil
	}

	if name, found := strings.CutPrefix(variable, "resolver:"); found {
		resolved, err := r.resolve(name)
		if err != nil {
			return "", fail("could not be resolved", err)
		}

		return resolved.Value, nil
	}

	return "", fail("is not a known variable", nil)
}

// resolve calls the resolver name once per render and returns its value.
func (r *renderer) resolve(name string) (Resolved, error) {
	if resolved, found := r.resolved[name]; found {
		return resolved, nil
	}

	if r.input.Resolvers == nil {
		return Resolved{}, errNoResolvers
	}

	identity := r.input.Identity
	request := ResolveRequest{
		Resolver: name,
		Template: ResolveTemplate{Namespace: r.input.Template.Namespace, Name: r.input.Template.Name},
		Instance: r.input.Instance.ID,
		Principal: ResolvePrincipal{
			ID: identity.PrincipalID, Email: identity.PrincipalEmail, Name: identity.PrincipalName,
			Groups: identity.PrincipalGroups,
		},
	}

	if r.input.Template.Spec.Scope == scopePerSession {
		request.Workspace = identity.WorkspaceID
		request.Session = identity.SessionID
	}

	resolved, err := r.input.Resolvers.Resolve(r.ctx, request)
	if err != nil {
		return Resolved{}, err
	}

	if r.resolved == nil {
		r.resolved = map[string]Resolved{}
	}

	r.resolved[name] = resolved

	return resolved, nil
}

// isPerIdentityVariable reports whether variable depends on a principal, a
// workspace, a session or a user's secret, none of which a shared instance
// has.
func isPerIdentityVariable(variable string) bool {
	for _, prefix := range []string{"principal.", "workspace.", "session.", "secret:", "resolver:"} {
		if strings.HasPrefix(variable, prefix) {
			return true
		}
	}

	return false
}

// claimed returns a claimed value as it renders at path: hashed when path is
// a name or a label value, so it fits Kubernetes' rules, and as it is
// elsewhere.
func claimed(value string, path []string) string {
	if !isNameOrLabel(path) {
		return value
	}

	return hashClaimed(value)
}

// hashClaimed hashes a value so it fits a Kubernetes name or label value.
func hashClaimed(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:])[:claimedHashLength]
}

// isNameOrLabel reports whether path is an object name, a reference to one,
// or a label value. Names are fields called name or ending in Name
// (metadata.name, serviceAccountName, secretKeyRef.name, …). Label values are
// the values of labels, matchLabels and selector maps, so selectors and the
// labels they select stay equal.
func isNameOrLabel(path []string) bool {
	if len(path) == 0 {
		return false
	}

	key := path[len(path)-1]
	if key == "name" || strings.HasSuffix(key, "Name") {
		return true
	}

	if len(path) < 2 {
		return false
	}

	switch path[len(path)-2] {
	case "labels", "matchLabels", "selector":
		return true
	}

	return false
}

func formatPath(path []string) string {
	var out strings.Builder

	for index, part := range path {
		if index > 0 && !strings.HasPrefix(part, "[") {
			out.WriteByte('.')
		}

		out.WriteString(part)
	}

	return out.String()
}

// stampPodTemplate puts the instance metadata on the pod template of a
// workload object, so its pods carry it too. A template's own keys are kept
// unless they are toolgate's. The instance, template and managed-by labels
// stay off the pods: the reconciler judges every pod that carries the
// instance label, and a pod that is not ready yet would fail the instance.
func stampPodTemplate(object *unstructured.Unstructured, meta InstanceMetadata) {
	if _, found, _ := unstructured.NestedMap(object.Object, "spec", "template"); !found {
		return
	}

	labels, _, _ := unstructured.NestedStringMap(object.Object, "spec", "template", "metadata", "labels")
	labels = mergeKeys(labels, meta.Labels)

	annotations, _, _ := unstructured.NestedStringMap(object.Object, "spec", "template", "metadata", "annotations")
	annotations = mergeKeys(annotations, meta.Annotations)

	_ = unstructured.SetNestedStringMap(object.Object, labels, "spec", "template", "metadata", "labels")
	if len(annotations) > 0 {
		_ = unstructured.SetNestedStringMap(object.Object, annotations, "spec", "template", "metadata", "annotations")
	}
}
