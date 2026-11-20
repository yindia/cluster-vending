package cluster

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	"github.com/stretchr/testify/require"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/helmrel"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// liveComponent is a platform component of the cluster seen live.
type liveComponent struct {
	name     string
	sequence int32
	requires []string
}

// liveComponents is the set of the cluster that deadlocked: Karpenter
// (sequence 0), the cluster autoscaler (10, as it was then), and the rest at 100.
var liveComponents = []liveComponent{
	{"karpenter", 0, nil},
	{"cluster-autoscaler", 10, nil},
	{"capsule", 100, nil},
	{"aws-load-balancer-controller", 100, nil},
	{"istio-base", 100, nil},
	{"istiod", 100, []string{"istio-base"}},
	{"istio-ingress-gateway", 100, []string{"istiod", "aws-load-balancer-controller"}},
	{"external-secrets", 100, nil},
	{"kyverno", 100, nil},
	{"ztunnel", 100, nil},
	{"istio-cni", 100, nil},
}

// usageWebhook is Crossplane's Usage protection webhook: an object a Usage
// names as "of" is refused its deletion (403, as the API server relays a
// denial) while the Usage's "by" object exists.
func usageWebhook(ctx context.Context, c client.Reader, obj client.Object) error {
	if _, ok := obj.(*protectionv1beta1.Usage); ok {
		return nil
	}
	usages := &protectionv1beta1.UsageList{}
	if err := c.List(ctx, usages, client.InNamespace(obj.GetNamespace())); err != nil {
		return err
	}
	for _, u := range usages.Items {
		if u.Spec.Of.Kind != gvkOf(obj).Kind || u.Spec.Of.ResourceRef == nil || u.Spec.Of.ResourceRef.Name != obj.GetName() ||
			u.Spec.By == nil || u.Spec.By.ResourceRef == nil {
			continue
		}
		by := &unstructured.Unstructured{}
		gv, _ := schema.ParseGroupVersion(u.Spec.By.APIVersion)
		by.SetGroupVersionKind(gv.WithKind(u.Spec.By.Kind))
		if err := c.Get(ctx, client.ObjectKey{Namespace: u.Namespace, Name: u.Spec.By.ResourceRef.Name}, by); err != nil {
			continue
		}
		return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: 403, Reason: metav1.StatusReasonForbidden,
			Message: fmt.Sprintf(`admission webhook "nousages.protection.crossplane.io" denied the request: This resource is in-use by 1 usage(s), `+
				`including the *v1beta1.Usage %q (of %s %s) by resource %s/%s.`, u.Name, gvkOf(obj).Kind, obj.GetName(), u.Spec.By.Kind, u.Spec.By.ResourceRef.Name)}}
	}
	return nil
}

// newLiveDelOrder is a DELETING cluster with the live components, every
// Release held by the provider, the Usages the platform renders between
// them (a component's Release is used by the Releases of every component
// of a higher sequence and of those requiring it), the usage webhook, and
// the EKS cluster.
func newLiveDelOrder(t *testing.T) *delOrder {
	t.Helper()
	d := &delOrder{t: t, holds: map[string]int{}, deleting: map[string]int{}, td: &fakeTeardowns{}}
	var defs []cloudv1alpha1.ComponentDefinition
	for _, c := range liveComponents {
		def := platformDef(c.name, c.requires...)
		def.Spec.Sequence = ptr.To(c.sequence)
		defs = append(defs, def)
	}
	d.src = &fakePlatform{offered: "2026-11-a", defs: defs}
	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if err := d.webhook(ctx, c, obj); err != nil {
			d.refused = append(d.refused, gvkOf(obj).Kind+" "+obj.GetName())
			return err
		}
		return c.Delete(ctx, obj, opts...)
	}}
	d.webhook = usageWebhook
	d.world = newWorld(t, funcs, nil, func(deps *reconcile.Deps) { deps.Platform, deps.Teardowns = d.src, d.td })
	d.prefix = crossplaneaws.ClusterPrefix(worldTenant, d.row.ID, d.row.Name)
	for _, c := range liveComponents {
		d.platformComponent(c.name, rankBase)
	}
	release := func(name string) crossplaneaws.UsageRef {
		return crossplaneaws.UsageRef{APIVersion: crossplaneaws.HelmReleaseGVK.GroupVersion().String(),
			Kind: crossplaneaws.HelmReleaseGVK.Kind, Name: crossplaneaws.HelmReleaseObjectName(d.prefix, name)}
	}
	var pairs []crossplaneaws.UsagePair
	for _, c := range liveComponents {
		for _, lower := range liveComponents {
			if lower.sequence < c.sequence {
				pairs = append(pairs, crossplaneaws.UsagePair{Of: release(lower.name), By: release(c.name)})
			}
		}
		for _, req := range c.requires {
			pairs = append(pairs, crossplaneaws.UsagePair{Of: release(req), By: release(c.name)})
		}
	}
	for _, u := range crossplaneaws.Usages(worldNS, d.prefix, crossplane.ClusterLabels(worldTenant, d.row.ID), pairs) {
		require.NoError(t, d.kube.Create(t.Context(), u))
		d.record(model.OwnerClusterComponents, d.row.ID, protectionv1beta1.SchemeGroupVersion.WithKind(protectionv1beta1.UsageKind), u.Name)
	}
	d.holdExisting(&awseks.Cluster{}, d.prefix)
	d.holdExisting(&awseks.ClusterAuth{}, d.prefix+"-auth")
	d.record(model.OwnerCluster, d.row.ID, awseks.CRDGroupVersion.WithKind(awseks.ClusterAuth_Kind), d.prefix+"-auth")
	d.deleteCluster()
	return d
}

// started reports whether the Release of the chart is being deleted or gone.
func (d *delOrder) started(chart string) bool {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	err := d.kube.Get(d.t.Context(), client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.HelmReleaseObjectName(d.prefix, chart)}, u)
	return err != nil || u.GetDeletionTimestamp() != nil
}

// The cluster that deadlocked: every Release goes in the order the Usages
// enforce, the webhook is never asked for a deletion it would refuse, and the
// deletion completes.
func TestDeletionNeverSendsADeleteTheUsageWebhookRefuses(t *testing.T) {
	d := newLiveDelOrder(t)
	for _, c := range liveComponents {
		d.holds[crossplaneaws.HelmReleaseObjectName(d.prefix, c.name)] = 1
	}

	messages := d.run(120)

	require.Empty(t, d.refused, "the platform sent deletes the Usage webhook refused")
	require.True(t, d.gone(), "the deletion completes; last message %q", messages[len(messages)-1])
	joined := strings.Join(messages, "\n")
	require.Contains(t, joined, "stage 3 of 7")
	require.Contains(t, joined, "aws-load-balancer-controller waits for the components that require it")
	require.NotContains(t, joined, "could not apply", "waiting for users is not a failure")
}

// Within a sequence a Release goes only once no Release that uses it exists,
// and Karpenter's goes in stage 5, after every other.
func TestDeletionOrdersReleasesByTheirUsages(t *testing.T) {
	d := newLiveDelOrder(t)
	var order []string
	d.webhook = func(ctx context.Context, c client.Reader, obj client.Object) error {
		if err := usageWebhook(ctx, c, obj); err != nil {
			return err
		}
		if gvkOf(obj).Kind == crossplaneaws.HelmReleaseGVK.Kind {
			order = append(order, strings.TrimPrefix(obj.GetName(), d.prefix+"-"))
		}
		return nil
	}

	d.run(120)

	require.True(t, d.gone())
	index := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		return -1
	}
	before := func(first, second string) {
		require.Less(t, index(first), index(second), "%s is deleted before %s: %v", first, second, order)
	}
	before("istio-ingress-gateway", "istiod")
	before("istio-ingress-gateway", "aws-load-balancer-controller")
	before("istiod", "istio-base")
	for _, c := range liveComponents {
		if c.sequence == 100 {
			before(c.name, "cluster-autoscaler")
		}
		if c.name != "karpenter" {
			before(c.name, "karpenter")
		}
	}
	before("cluster-autoscaler", "karpenter")
}

// A forced deletion lets the Releases go without an uninstall, and still never
// asks the webhook for a deletion it refuses.
func TestForcedDeletionRespectsTheUsages(t *testing.T) {
	d := newLiveDelOrder(t)
	row := d.cluster(t)
	row.DeleteForce = true
	require.NoError(t, d.store.Clusters().Update(t.Context(), row))

	d.run(120)

	require.Empty(t, d.refused)
	require.True(t, d.gone())
}

// A refusal by the webhook that gets through (a Usage the platform did not
// know) is "waiting for its users", not a failure of the pass.
func TestUsageRefusalIsNotAnError(t *testing.T) {
	d := newLiveDelOrder(t)
	d.webhook = func(ctx context.Context, c client.Reader, obj client.Object) error {
		if obj.GetName() == crossplaneaws.HelmReleaseObjectName(d.prefix, "kyverno") && !d.started("capsule") {
			return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: 403, Reason: metav1.StatusReasonForbidden,
				Message: `admission webhook "nousages.protection.crossplane.io" denied the request: This resource is in-use by 1 usage(s)`}}
		}
		return usageWebhook(ctx, c, obj)
	}

	for range 3 {
		require.NoError(t, d.svc.Reconcile(t.Context(), worldTenant, d.row.ID), "a refused delete does not fail the pass")
		d.provider()
	}
	d.webhook = usageWebhook
	d.run(120)
	require.True(t, d.gone())
}

// The platform's own backoff pause is lifted from every Release at the start
// of a pass, whichever component the pass gets to; an operator's pause stays
// and is reported.
func TestDeletionLiftsEveryPlatformPauseFirst(t *testing.T) {
	d := newLiveDelOrder(t)
	for _, c := range []string{"capsule", "istio-ingress-gateway"} {
		d.holds[crossplaneaws.HelmReleaseObjectName(d.prefix, c)] = 50
	}
	backoff := map[string]string{managed.PausedAnnotation: "true",
		helmrel.BackoffAnnotation: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), helmrel.GaveUpAnnotation: "true"}
	with := func(chart string, extra map[string]string) {
		annotations := d.releaseOf(chart).GetAnnotations()
		for k, v := range extra {
			annotations[k] = v
		}
		d.annotate(chart, annotations)
	}
	with("capsule", backoff)
	with("istio-ingress-gateway", backoff)
	with("karpenter", backoff)                                           // a component far from this pass
	with("kyverno", map[string]string{managed.PausedAnnotation: "true"}) // an operator's
	require.NoError(t, d.kube.Delete(t.Context(), d.releaseOf("capsule")))

	msg := d.pass()

	for _, chart := range []string{"capsule", "istio-ingress-gateway", "karpenter"} {
		a := d.releaseOf(chart).GetAnnotations()
		require.NotContains(t, a, managed.PausedAnnotation, chart)
		require.NotContains(t, a, helmrel.BackoffAnnotation, chart)
		require.NotContains(t, a, helmrel.GaveUpAnnotation, chart)
	}
	require.Equal(t, "true", d.releaseOf("kyverno").GetAnnotations()[managed.PausedAnnotation], "an operator's pause stays")
	require.Contains(t, msg, "kyverno")
	require.Contains(t, msg, "paused (crossplane.io/paused) by an operator or a freeze")
}

// The Usages alone order the deletion: with a catalog that names no
// requirement, a Release waits for what uses it, and says so.
func TestDeletionWaitsForUsersNamedOnlyByUsages(t *testing.T) {
	d := newLiveDelOrder(t)
	for i := range d.src.defs {
		d.src.defs[i].Spec.Requires = nil
	}

	messages := d.run(120)

	require.Empty(t, d.refused)
	require.True(t, d.gone())
	joined := strings.Join(messages, "\n")
	require.Contains(t, joined, "aws-load-balancer-controller waits for istio-ingress-gateway (Usage)")
	require.Contains(t, joined, "istiod waits for istio-ingress-gateway (Usage)")
	require.NotContains(t, joined, "could not apply")
}
