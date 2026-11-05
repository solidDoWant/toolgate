package catalog

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// defaultLoadPollInterval is how often Loader checks the catalog against
// the templates in the watched namespaces.
const defaultLoadPollInterval = 100 * time.Millisecond

// Loader marks a Catalog loaded once it holds every template in the watched
// namespaces, at the generation the Reader lists. It is a manager runnable
// that runs on every replica.
type Loader struct {
	// Reader lists the templates, normally the manager's cache: its List
	// waits until the informer has synced.
	Reader  client.Reader
	Catalog *Catalog
	// Namespaces are the watched namespaces.
	Namespaces []string
	// PollInterval between checks. Zero means 100 ms.
	PollInterval time.Duration
}

// Start checks the catalog until it holds every listed template, marks it
// loaded and returns. It returns early with an error only when ctx ends.
func (l *Loader) Start(ctx context.Context) error {
	interval := l.PollInterval
	if interval == 0 {
		interval = defaultLoadPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if l.complete(ctx) {
			l.Catalog.MarkLoaded()

			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("catalog: not loaded: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// NeedLeaderElection makes the manager run the loader on every replica.
func (l *Loader) NeedLeaderElection() bool { return false }

// complete reports whether the catalog holds every template the Reader
// lists, at its listed generation or a later one. A failed list counts as
// not complete; the next check lists again.
func (l *Loader) complete(ctx context.Context) bool {
	for _, namespace := range l.Namespaces {
		var templates v1alpha1.MCPServerTemplateList
		if err := l.Reader.List(ctx, &templates, client.InNamespace(namespace)); err != nil {
			return false
		}

		for index := range templates.Items {
			template := &templates.Items[index]
			if !template.DeletionTimestamp.IsZero() {
				continue
			}

			status, found := l.Catalog.Status(types.NamespacedName{Namespace: template.Namespace, Name: template.Name})
			if !found || status.Generation < template.Generation {
				return false
			}
		}
	}

	return true
}
