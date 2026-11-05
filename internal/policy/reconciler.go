package policy

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
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
// but the status subresource.
type RuleReconciler struct {
	Client client.Client
	Rules  *Rules
	Log    *slog.Logger
	// Namespaces are the namespaces whose rules are read. Rules in other
	// namespaces are ignored.
	Namespaces []string
}

// SetupWithManager registers the reconciler with mgr.
func (r *RuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if len(r.Namespaces) == 0 {
		return fmt.Errorf("policy: no namespaces to watch")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ToolgateRule{}, builder.WithPredicates(predicate.NewPredicateFuncs(r.watched))).
		Named("rules").
		Complete(r)
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

	if !meta.SetStatusCondition(&rule.Status.Conditions, condition) {
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
