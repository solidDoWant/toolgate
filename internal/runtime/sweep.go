package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Sweep reconciles the instance objects in Kubernetes with the instance
// rows after a leader change or a gateway restart (docs/design/phase-1.md
// §4, row 17). It works on instance namespaces (`tg-<id>`): deleting one
// deletes all of its instance's objects.
//
// It lists the labelled namespaces first and reads the rows second, so a
// namespace applied after the rows were read can't be taken for an orphan.
type Sweep struct {
	Pool *pgxpool.Pool
	// Reader lists instance namespaces. It must read the API server, not a
	// cache that may lag behind it.
	Reader  client.Reader
	Deleter Deleter
	// Templates gives the template a starting or ready instance without
	// objects is rendered from again. Nil means no row is sent back to
	// pending.
	Templates Templates
	Log       *slog.Logger
}

// Pass wraps a full pass so that the sweep runs before it.
func (s *Sweep) Pass(pass func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := s.Run(ctx); err != nil {
			return errors.Join(err, pass(ctx))
		}

		return pass(ctx)
	}
}

// Run deletes every labelled namespace that belongs to no live instance, and
// sends every live starting or ready instance whose namespace is missing back
// to pending at its template's current generation.
func (s *Sweep) Run(ctx context.Context) error {
	namespaces := &corev1.NamespaceList{}
	if err := s.Reader.List(ctx, namespaces, client.MatchingLabels{LabelManagedBy: ManagedByValue},
		client.HasLabels{LabelInstance}); err != nil {
		return fmt.Errorf("list instance namespaces: %w", err)
	}

	instances, err := db.New(s.Pool).ListInstancesNotStopped(ctx)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	live := make(map[string]db.Instance, len(instances))

	for _, instance := range instances {
		if isLive(instance) {
			live[instance.ID] = instance
		}
	}

	present := make(map[string]bool, len(namespaces.Items))

	var problems []error

	for index := range namespaces.Items {
		namespace := &namespaces.Items[index]
		id := namespace.Labels[LabelInstance]

		if instance, found := live[id]; found && instance.Namespace == namespace.Name {
			present[id] = true

			continue
		}

		if namespace.DeletionTimestamp != nil {
			continue
		}

		s.Log.InfoContext(ctx, "deleting orphaned instance namespace", slog.String("namespace", namespace.Name),
			slog.String("instance", id))

		if err := s.Deleter.Delete(ctx, namespace, client.Preconditions{UID: &namespace.UID}); err != nil &&
			!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			problems = append(problems, err)
		}
	}

	for _, instance := range live {
		if present[instance.ID] || (instance.Status != StatusStarting && instance.Status != statusReady) {
			continue
		}

		// An instance in a namespace it shares with others is judged by
		// its namespace's objects, which the sweep doesn't list.
		if instance.Namespace != NamespacePrefix+instance.ID {
			continue
		}

		if err := s.restart(ctx, instance); err != nil {
			problems = append(problems, err)
		}
	}

	return errors.Join(problems...)
}

// restart sends an instance whose objects are missing back to pending, at
// the generation of the template it is rendered from.
func (s *Sweep) restart(ctx context.Context, instance db.Instance) error {
	if s.Templates == nil {
		return nil
	}

	template, found := s.Templates.Running(types.NamespacedName{Namespace: instance.TemplateNs, Name: instance.TemplateName})
	if !found || string(template.UID) != instance.TemplateUid {
		// The stop decisions handle a deleted or re-created template.
		return nil
	}

	s.Log.InfoContext(ctx, "instance objects are missing; starting it again", slog.String("instance", instance.ID),
		slog.String("status", instance.Status))

	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		changed, err := db.New(tx).SetInstancePending(ctx, db.SetInstancePendingParams{
			ID: instance.ID, FromStatus: instance.Status, Generation: template.Generation,
		})
		if err != nil || changed == 0 {
			return err
		}

		return store.Notify(ctx, tx, store.KindInstance, instance.ID)
	})
	if err != nil {
		return fmt.Errorf("instance %s: back to pending: %w", instance.ID, err)
	}

	return nil
}
