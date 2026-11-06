package catalog

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/changes"
)

// ErrNotFound is returned for a server name no watched template has.
var ErrNotFound = errors.New("no such server")

// duplicateReason is the reason given when valid templates in different
// namespaces share a server name.
const duplicateReason = "duplicate server name"

// InvalidError is returned for a server whose template is invalid. Reason is
// the validation reason, also written to the template's status.
type InvalidError struct {
	Server string
	Reason string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("the template of server %q is invalid: %s", e.Server, e.Reason)
}

// Status is the validation result of one template generation.
type Status struct {
	// Generation is the template generation the result is for.
	Generation int64
	// Valid is true when the template is offered.
	Valid bool
	// Reason says why the template is invalid. It is empty when Valid.
	Reason string
}

// Catalog holds the templates read from the watched namespaces. It offers only
// templates that are valid now, and it keeps each template's last valid
// generation for the instances already running from it. It is safe for
// concurrent use.
type Catalog struct {
	mu      sync.RWMutex
	entries map[types.NamespacedName]*entry

	loaded     chan struct{}
	loadedOnce sync.Once

	changes changes.Feed
}

type entry struct {
	// current is the latest generation read.
	current *v1alpha1.MCPServerTemplate
	// problems are current's own validation problems, without the
	// duplicate-name check.
	problems []string
	// lastValid is the latest generation that was valid, or nil.
	lastValid *v1alpha1.MCPServerTemplate
}

// New returns an empty catalog.
func New() *Catalog {
	return &Catalog{entries: map[types.NamespacedName]*entry{}, loaded: make(chan struct{})}
}

// MarkLoaded records that the catalog holds every template in the watched
// namespaces, as Loader does once it has seen them all. Later calls do
// nothing.
func (c *Catalog) MarkLoaded() {
	c.loadedOnce.Do(func() { close(c.loaded) })
}

// Loaded returns a channel that is closed once MarkLoaded was called. Until
// then the catalog may be missing templates, and judging instances against
// it would stop instances whose template exists.
func (c *Catalog) Loaded() <-chan struct{} {
	return c.loaded
}

// IsLoaded reports whether MarkLoaded was called.
func (c *Catalog) IsLoaded() bool {
	select {
	case <-c.loaded:
		return true
	default:
		return false
	}
}

// Set records the latest generation of a template and its own validation
// problems. It returns the templates whose status may have changed: the
// template itself and every other template with the same server name, before
// or after the change.
func (c *Catalog) Set(template *v1alpha1.MCPServerTemplate, problems []string) []types.NamespacedName {
	key := types.NamespacedName{Namespace: template.Namespace, Name: template.Name}

	defer c.changes.Notify()

	c.mu.Lock()
	defer c.mu.Unlock()

	current, found := c.entries[key]
	if !found {
		current = &entry{}
		c.entries[key] = current
	}

	current.current = template.DeepCopy()
	current.problems = slices.Clone(problems)

	return c.refresh(template.Name)
}

// Delete forgets a template. It returns the other templates whose status may
// have changed.
func (c *Catalog) Delete(key types.NamespacedName) []types.NamespacedName {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, found := c.entries[key]; !found {
		return nil
	}

	defer c.changes.Notify()

	delete(c.entries, key)

	return c.refresh(key.Name)
}

// Subscribe returns a channel that receives after a template is set or
// deleted, and a function that ends the subscription. Signals not yet
// received are merged into one.
func (c *Catalog) Subscribe() (<-chan struct{}, func()) {
	return c.changes.Subscribe()
}

// refresh updates the last valid generation of every template named name and
// returns their keys. c.mu must be held.
func (c *Catalog) refresh(name string) []types.NamespacedName {
	keys := c.keysNamed(name)

	for _, key := range keys {
		current := c.entries[key]
		if c.statusLocked(key, current).Valid {
			current.lastValid = current.current
		}
	}

	return keys
}

// keysNamed returns the keys of the templates with server name name, sorted.
func (c *Catalog) keysNamed(name string) []types.NamespacedName {
	var keys []types.NamespacedName

	for key := range c.entries {
		if key.Name == name {
			keys = append(keys, key)
		}
	}

	slices.SortFunc(keys, func(a, b types.NamespacedName) int {
		return strings.Compare(a.String(), b.String())
	})

	return keys
}

// statusLocked computes the status of one entry. c.mu must be held.
func (c *Catalog) statusLocked(key types.NamespacedName, current *entry) Status {
	status := Status{Generation: current.current.Generation}

	if len(current.problems) > 0 {
		status.Reason = Reason(current.problems)

		return status
	}

	for other, otherEntry := range c.entries {
		if other != key && other.Name == key.Name && len(otherEntry.problems) == 0 {
			status.Reason = duplicateReason

			return status
		}
	}

	status.Valid = true

	return status
}

// Status returns the validation result of the template's latest generation.
func (c *Catalog) Status(key types.NamespacedName) (Status, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	current, found := c.entries[key]
	if !found {
		return Status{}, false
	}

	return c.statusLocked(key, current), true
}

// Offered returns the template of the named server, if it is valid now. For
// an invalid template it returns an *InvalidError with the reason, so new
// enables are refused with it. A copy is returned.
func (c *Catalog) Offered(server string) (*v1alpha1.MCPServerTemplate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	keys := c.keysNamed(server)
	if len(keys) == 0 {
		return nil, ErrNotFound
	}

	var reasons []string

	for _, key := range keys {
		current := c.entries[key]

		status := c.statusLocked(key, current)
		if status.Valid {
			return current.current.DeepCopy(), nil
		}

		reasons = append(reasons, status.Reason)
	}

	return nil, &InvalidError{Server: server, Reason: reasons[0]}
}

// List returns every template that is valid now, sorted by server name.
// Copies are returned.
func (c *Catalog) List() []*v1alpha1.MCPServerTemplate {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var templates []*v1alpha1.MCPServerTemplate

	for key, current := range c.entries {
		if c.statusLocked(key, current).Valid {
			templates = append(templates, current.current.DeepCopy())
		}
	}

	slices.SortFunc(templates, func(a, b *v1alpha1.MCPServerTemplate) int {
		return strings.Compare(a.Name, b.Name)
	})

	return templates
}

// UIDs returns the UID of every template read, valid or not.
func (c *Catalog) UIDs() map[string]bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	uids := make(map[string]bool, len(c.entries))
	for _, current := range c.entries {
		uids[string(current.current.UID)] = true
	}

	return uids
}

// Running returns the last valid generation of a template, for the instances
// already running from it. It stays available while the template is invalid,
// and is gone once the template is deleted. A copy is returned.
func (c *Catalog) Running(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	current, found := c.entries[key]
	if !found || current.lastValid == nil || key.Name == ReservedServerName {
		return nil, false
	}

	return current.lastValid.DeepCopy(), true
}

// LastValidByUID returns a copy of the last valid generation of every
// template that has one, keyed by UID.
func (c *Catalog) LastValidByUID() map[string]*v1alpha1.MCPServerTemplate {
	c.mu.RLock()
	defer c.mu.RUnlock()

	valid := make(map[string]*v1alpha1.MCPServerTemplate, len(c.entries))

	for key, current := range c.entries {
		if current.lastValid != nil && key.Name != ReservedServerName {
			valid[string(current.lastValid.UID)] = current.lastValid.DeepCopy()
		}
	}

	return valid
}
