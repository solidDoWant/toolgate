package policy

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/election"
)

// Condition type and reasons written to a rule's status.
const (
	ConditionValid = "Valid"

	ReasonValid   = "Valid"
	ReasonInvalid = "Invalid"
)

// maxConditionMessage bounds the reason written to a rule's status.
const maxConditionMessage = 4096

// RuleReconciler reads ToolgateRule objects from the informer cache into
// Rules and writes the validation result to their status. It writes nothing
// but the status subresource. It runs on every replica, because each
// replica's policy reads its own rules, and writes status only while Elected
// is closed.
type RuleReconciler struct {
	Client client.Client
	Rules  *Rules
	Log    *slog.Logger
	// Elected is closed once this replica holds the leader Lease
	// (manager.Manager.Elected). Nil means the reconciler always writes
	// status.
	Elected <-chan struct{}
	// Namespaces are the namespaces whose rules are read. Rules in other
	// namespaces are ignored.
	Namespaces []string
}

// SetupWithManager registers the reconciler with mgr.
func (r *RuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if len(r.Namespaces) == 0 {
		return fmt.Errorf("policy: no namespaces to watch")
	}

	requeue, err := election.RequeueOnElection(mgr, r.listWatched)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ToolgateRule{}, builder.WithPredicates(predicate.NewPredicateFuncs(r.watched))).
		WatchesRawSource(requeue).
		WithOptions(controller.Options{NeedLeaderElection: ptr.To(false)}).
		Named("rules").
		Complete(r)
}

// listWatched returns every rule in the watched namespaces.
func (r *RuleReconciler) listWatched(ctx context.Context) ([]client.Object, error) {
	var objects []client.Object

	for _, namespace := range r.Namespaces {
		var rules v1alpha1.ToolgateRuleList
		if err := r.Client.List(ctx, &rules, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("policy: listing rules in %s: %w", namespace, err)
		}

		for index := range rules.Items {
			objects = append(objects, &rules.Items[index])
		}
	}

	return objects, nil
}

func (r *RuleReconciler) watched(object client.Object) bool {
	return slices.Contains(r.Namespaces, object.GetNamespace())
}

// Reconcile reads one rule into Rules and writes its Valid condition.
func (r *RuleReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if !slices.Contains(r.Namespaces, request.Namespace) {
		return reconcile.Result{}, nil
	}

	var rule v1alpha1.ToolgateRule

	err := r.Client.Get(ctx, request.NamespacedName, &rule)

	switch {
	case apierrors.IsNotFound(err):
		r.Rules.Delete(request.NamespacedName)

		return reconcile.Result{}, nil
	case err != nil:
		return reconcile.Result{}, fmt.Errorf("policy: reading rule %s: %w", request.NamespacedName, err)
	case !rule.DeletionTimestamp.IsZero():
		r.Rules.Delete(request.NamespacedName)

		return reconcile.Result{}, nil
	}

	condition := metav1.Condition{
		Type:               ConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonValid,
		Message:            "The rule is valid and applies.",
		ObservedGeneration: rule.Generation,
	}

	if err := r.Rules.Set(&rule); err != nil {
		message := err.Error()
		if len(message) > maxConditionMessage {
			message = message[:maxConditionMessage]
		}

		condition.Status = metav1.ConditionFalse
		condition.Reason = ReasonInvalid
		condition.Message = message

		r.logger().WarnContext(ctx, "rule is invalid", "namespace", rule.Namespace, "rule", rule.Name, "generation", rule.Generation, "reason", message)
	}

	if !election.IsElected(r.Elected) || !meta.SetStatusCondition(&rule.Status.Conditions, condition) {
		return reconcile.Result{}, nil
	}

	if err := r.Client.Status().Update(ctx, &rule); err != nil {
		return reconcile.Result{}, fmt.Errorf("policy: writing the status of rule %s: %w", request.NamespacedName, err)
	}

	return reconcile.Result{}, nil
}

func (r *RuleReconciler) logger() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}

	return r.Log
}
