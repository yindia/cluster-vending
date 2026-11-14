package controller

import (
	"context"
	"strings"
	"time"

	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
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
	// recorder reports a forced delete; nil records nothing.
	recorder events.EventRecorder
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
	force, invalid := component.ForceDelete(def.Annotations)
	if deleting && len(held) > 0 && force {
		// The operator's last resort: the objects stay on those clusters.
		ignored := strings.Join(held, "; ")
		log.FromContext(ctx).Info("deleting a component definition still in use (force-delete); its objects stay on those clusters",
			"definition", def.Name, "ignored", ignored)
		if r.recorder != nil {
			r.recorder.Eventf(def, nil, "Warning", "ForceDeleted", eventAction,
				"deleted although in use (%s); what it installed stays there: %s", component.AnnotationForceDelete, ignored)
		}
		held = nil
	}
	if invalid != "" {
		held = append(held, invalid)
	}
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
	objects, err := r.componentObjects(ctx, installed)
	if err != nil {
		return nil, err
	}
	clusters := &cloudv1alpha1.ClusterList{}
	if err := r.c.List(ctx, clusters); err != nil {
		return nil, err
	}
	states := make([]component.ClusterState, 0, len(clusters.Items))
	for i := range clusters.Items {
		cl := &clusters.Items[i]
		state := component.ClusterState{ID: cl.Labels[cloudstore.LabelID], Where: where(cl.Labels[cloudstore.LabelTenant], cl.Namespace, cl.Name),
			Deleting: !cl.DeletionTimestamp.IsZero() || cl.Status.Phase == cloudv1alpha1.ClusterPhaseDeleting}
		if p := model.ParsePause(cl.Annotations[cloudstore.AnnotationPaused]); p != nil {
			state.PausedBy, state.PausedSince = p.By, p.Since
		}
		states = append(states, state)
	}
	use.Clusters = component.HoldingClusters(installed, objects, states)
	return component.HeldBy(def.Name, catalog, use), nil
}

// componentObjects are the objects of the installed components in the
// management cluster: the Helm Releases and IAM Roles the installer
// rendered for them, by their component label. They are what an uninstall
// removes, and they go exactly when it is done; a cluster's recorded status
// would say the same only while the cluster keeps reporting it.
func (r *ComponentDefinitionReconciler) componentObjects(ctx context.Context, installed []string) ([]component.Recorded, error) {
	of, err := labels.NewRequirement(crossplane.LabelComponent, selection.In, installed)
	if err != nil {
		return nil, err
	}
	selector := client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(*of)}
	releases := &unstructured.UnstructuredList{}
	releases.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK.GroupVersion().WithKind(crossplaneaws.HelmReleaseGVK.Kind + "List"))
	if err := r.c.List(ctx, releases, selector); err != nil && !meta.IsNoMatchError(err) {
		return nil, err
	}
	// The manager's cache holds managed resources as unstructured objects
	// only (kube.NewManagerWith): a typed list of them is refused.
	roles := &unstructured.UnstructuredList{}
	roles.SetGroupVersionKind(awsiam.CRDGroupVersion.WithKind(awsiam.Role_Kind + "List"))
	if err := r.c.List(ctx, roles, selector); err != nil && !meta.IsNoMatchError(err) {
		return nil, err
	}
	out := make([]component.Recorded, 0, len(releases.Items)+len(roles.Items))
	for i := range releases.Items {
		l := releases.Items[i].GetLabels()
		out = append(out, component.Recorded{Component: l[crossplane.LabelComponent], ClusterID: l[crossplane.LabelCluster]})
	}
	for i := range roles.Items {
		l := roles.Items[i].GetLabels()
		out = append(out, component.Recorded{Component: l[crossplane.LabelComponent], ClusterID: l[crossplane.LabelCluster]})
	}
	return out, nil
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
		Complete(&ComponentDefinitionReconciler{c: c, recorder: mgr.GetEventRecorder(EventsName)})
}
