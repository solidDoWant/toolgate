// Package runtime renders MCPServerTemplate resources into the Kubernetes
// objects of one instance (SPEC.md §9).
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

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
	WorkspaceID    string
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

// SecretSource resolves ${secret:…} and ${resolver:…} for the identity being
// rendered. The secrets package implements it.
type SecretSource interface {
	// Secret returns the value of the user's secret name.
	Secret(ctx context.Context, name string) (string, error)
	// Resolve returns a value from the resolver name.
	Resolve(ctx context.Context, name string) (string, error)
}

// Input is everything one render needs.
type Input struct {
	Template *v1alpha1.MCPServerTemplate
	Identity Identity
	Instance Instance
	Secrets  SecretSource
}

// Result is a rendered instance.
type Result struct {
	// Objects are the rendered objects, in template order.
	Objects []*unstructured.Unstructured
	// SecretsUsed are the names of the user secrets the render used, sorted.
	SecretsUsed []string
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
	renderer := &renderer{ctx: ctx, input: input, secretsUsed: map[string]bool{}}

	result := &Result{}

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

		labels := decoded.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}

		labels[LabelInstance] = input.Instance.ID
		labels[LabelTemplate] = input.Template.Name
		labels[LabelManagedBy] = ManagedByValue
		decoded.SetLabels(labels)

		result.Objects = append(result.Objects, decoded)
	}

	for name := range renderer.secretsUsed {
		result.SecretsUsed = append(result.SecretsUsed, name)
	}

	slices.Sort(result.SecretsUsed)

	return result, nil
}

type renderer struct {
	ctx         context.Context
	input       Input
	secretsUsed map[string]bool
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
		value, err := r.input.Secrets.Secret(r.ctx, name)
		if err != nil {
			return "", fail("could not be read", err)
		}

		r.secretsUsed[name] = true

		return value, nil
	}

	if name, found := strings.CutPrefix(variable, "resolver:"); found {
		value, err := r.input.Secrets.Resolve(r.ctx, name)
		if err != nil {
			return "", fail("could not be resolved", err)
		}

		return value, nil
	}

	return "", fail("is not a known variable", nil)
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
