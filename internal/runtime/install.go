package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// withoutLabel is a list option that keeps the objects that lack a label.
type withoutLabel string

// ApplyToList adds the requirement to the list's label selector.
func (w withoutLabel) ApplyToList(opts *client.ListOptions) {
	requirement, err := labels.NewRequirement(string(w), selection.DoesNotExist, nil)
	if err != nil {
		return
	}

	if opts.LabelSelector == nil {
		opts.LabelSelector = labels.NewSelector()
	}

	opts.LabelSelector = opts.LabelSelector.Add(*requirement)
}

// ownSelector selects the objects that carry the install's label. With no
// install ID it selects everything.
func (s *Sweep) ownSelector() client.ListOption {
	if s.InstallID == "" {
		return client.MatchingLabels{}
	}

	return client.MatchingLabels{LabelInstall: s.InstallID}
}

// adoptUnlabeled labels the shared-namespace objects that have no install
// label and belong to an instance this install has a row for. An unlabeled object no row names is never deleted: it is
// logged once as unowned. Instance namespaces are not labelled, because the
// gateway may not patch namespaces; the sweep matches them to rows instead.
func (s *Sweep) adoptUnlabeled(ctx context.Context) error {
	if s.InstallID == "" || s.Writer == nil {
		return nil
	}

	instances, err := db.New(s.Pool).ListInstancesNotStopped(ctx)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	namespaces := make(map[string]string, len(instances))
	for _, instance := range instances {
		namespaces[instance.ID] = instance.Namespace
	}

	if s.SharedNamespace == "" {
		return nil
	}

	var problems []error

	objects, err := listInstanceObjects(ctx, s.Reader, s.SharedNamespace, client.HasLabels{LabelInstance}, withoutLabel(LabelInstall))
	if err != nil {
		return errors.Join(append(problems, fmt.Errorf("list unlabeled instance objects: %w", err))...)
	}

	for _, object := range objects {
		id := object.GetLabels()[LabelInstance]

		if known, found := namespaces[id]; !found || known != s.SharedNamespace {
			s.logUnowned(ctx, object.GetKind(), object.GetNamespace(), object.GetName(), id)

			continue
		}

		if err := s.adopt(ctx, object); err != nil {
			problems = append(problems, err)
		}
	}

	return errors.Join(problems...)
}

// adopt adds the install label to an object.
func (s *Sweep) adopt(ctx context.Context, object client.Object) error {
	old, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}

	objectLabels := object.GetLabels()
	objectLabels[LabelInstall] = s.InstallID
	object.SetLabels(objectLabels)

	if err := s.Writer.Patch(ctx, object, client.MergeFrom(old)); err != nil {
		return fmt.Errorf("label %s with the install ID: %w", object.GetName(), err)
	}

	return nil
}

func (s *Sweep) logUnowned(ctx context.Context, kind, namespace, name, instance string) {
	key := kind + "/" + namespace + "/" + name
	if _, seen := s.unowned.LoadOrStore(key, struct{}{}); seen {
		return
	}

	s.Log.WarnContext(ctx, "found an unlabeled instance object that no instance of this install owns; leaving it alone",
		slog.String("kind", kind), slog.String("namespace", namespace), slog.String("name", name),
		slog.String("instance", instance))
}
