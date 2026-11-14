package controller

import (
	"context"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

// ConditionDefinitionInUse is a ComponentDefinition's condition: True
// while something uses it (component.HeldBy), with what.
const ConditionDefinitionInUse = "InUse"

// definitionRecheck is how often a definition whose delete is held by a
// cluster is looked at again: the cluster removes the component on its own
// schedule, and that is not an event of the definition.
const definitionRecheck = 30 * time.Second

// ComponentDefinitionReconciler keeps a ComponentDefinition while it is in
// use: a finalizer, held while a tenant Component of it exists, a cluster
// still has it installed, its bundle exists or another definition requires
// it; and an InUse condition that says which.
type ComponentDefinitionReconciler struct {
	c client.Client
}

// Reconcile adds the finalizer, records what uses the definition, and
// removes the finalizer of a deleted one once nothing does.
func (r *ComponentDefinitionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	def := &cloudv1alpha1.ComponentDefinition{}
	if err := r.c.Get(ctx, req.NamespacedName, def); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	held, err := r.heldBy(ctx, def)
	if err != nil {
		return ctrl.Result{}, err
	}
	deleting := !def.DeletionTimestamp.IsZero()
	if deleting && len(held) == 0 {
		if controllerutil.RemoveFinalizer(def, component.DefinitionFinalizer) {
			return ctrl.Result{}, client.IgnoreNotFound(r.c.Update(ctx, def))
		}
		return ctrl.Result{}, nil
	}
	if !deleting && controllerutil.AddFinalizer(def, component.DefinitionFinalizer) {
		if err := r.c.Update(ctx, def); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	status := *def.Status.DeepCopy()
	setDefinitionInUse(&status.Conditions, def.Generation, deleting, held)
	if !equality.Semantic.DeepEqual(status, def.Status) {
		def.Status = status
		if err := r.c.Status().Update(ctx, def); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if deleting {
		return ctrl.Result{RequeueAfter: definitionRecheck}, nil
	}
	return ctrl.Result{}, nil
}

// heldBy reads the catalog, the tenant Components and the clusters, and
// says what keeps def (component.HeldBy).
func (r *ComponentDefinitionReconciler) heldBy(ctx context.Context, def *cloudv1alpha1.ComponentDefinition) ([]string, error) {
	defs := &cloudv1alpha1.ComponentDefinitionList{}
	if err := r.c.List(ctx, defs); err != nil {
		return nil, err
	}
	catalog := make([]component.Definition, 0, len(defs.Items))
	for i := range defs.Items {
		d, err := component.DefinitionOf(&defs.Items[i])
		if err != nil {
			continue // unusable: holds nothing
		}
		catalog = append(catalog, d)
	}
	var use component.Use
	components := &cloudv1alpha1.ComponentList{}
	if err := r.c.List(ctx, components); err != nil {
		return nil, err
	}
	for i := range components.Items {
		if c := &components.Items[i]; c.Spec.Component == def.Name {
			use.Components = append(use.Components, where(c.Labels[cloudstore.LabelTenant], c.Namespace, c.Name))
		}
	}
	installed := append([]string{def.Name}, def.Spec.Includes...)
	clusters := &cloudv1alpha1.ClusterList{}
	if err := r.c.List(ctx, clusters); err != nil {
		return nil, err
	}
	for i := range clusters.Items {
		cl := &clusters.Items[i]
		if slices.ContainsFunc(cl.Status.PlatformComponents, func(p cloudv1alpha1.PlatformComponentStatus) bool {
			return slices.Contains(installed, p.Name)
		}) {
			use.Clusters = append(use.Clusters, where(cl.Labels[cloudstore.LabelTenant], cl.Namespace, cl.Name))
		}
	}
	return component.HeldBy(def.Name, catalog, use), nil
}

// where names an object of a tenant: "<tenant>/<name>", or the namespace
// when the object carries no tenant label.
func where(tenant, namespace, name string) string {
	if tenant == "" {
		tenant = namespace
	}
	return tenant + "/" + name
}

// setDefinitionInUse records what holds a definition.
func setDefinitionInUse(conds *[]metav1.Condition, generation int64, deleting bool, held []string) {
	if len(held) == 0 {
		meta.SetStatusCondition(conds, metav1.Condition{Type: ConditionDefinitionInUse, Status: metav1.ConditionFalse,
			Reason: "Unused", Message: "nothing uses the definition", ObservedGeneration: generation})
		return
	}
	msg, reason := strings.Join(held, "; "), "InUse"
	if deleting {
		msg, reason = "deletion waits: "+msg+" (disable a tenant's components first; clusters remove a platform one by themselves)",
			"DeletionBlocked"
	}
	meta.SetStatusCondition(conds, metav1.Condition{Type: ConditionDefinitionInUse, Status: metav1.ConditionTrue,
		Reason: reason, Message: msg, ObservedGeneration: generation})
}

// setupComponentDefinitions registers the controller: definitions are few,
// so a change of any Component or Cluster looks at every definition again.
func setupComponentDefinitions(mgr ctrl.Manager) error {
	c := mgr.GetClient()
	all := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return enqueueAll(ctx, c, &cloudv1alpha1.ComponentDefinitionList{})
	})
	return ctrl.NewControllerManagedBy(mgr).Named("componentdefinition").For(&cloudv1alpha1.ComponentDefinition{}).
		Watches(&cloudv1alpha1.ComponentDefinition{}, all).
		Watches(&cloudv1alpha1.Component{}, all).
		Watches(&cloudv1alpha1.Cluster{}, all).
		Complete(&ComponentDefinitionReconciler{c: c})
}
