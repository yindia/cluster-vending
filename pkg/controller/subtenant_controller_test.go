package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/naas"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	tenantlife "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
)

const stID = "00000000-0000-4000-8000-000000000021"
const stClusterID = "00000000-0000-4000-8000-000000000011"
const stNetworkID = "00000000-0000-4000-8000-000000000012"

type stClients struct {
	client client.Client
	calls  int
	err    error
}

func (s *stClients) Client(_ context.Context, namespace, secret string) (client.Client, error) {
	s.calls++
	if namespace != "tenant-acme" || secret != "cluster-kubeconfig" {
		return nil, fmt.Errorf("wrong workload client binding")
	}
	return s.client, s.err
}

type stInventory struct {
	client.Client
	contents map[string][]unstructured.Unstructured
	err      error
}

func (c *stInventory) NamespaceContents(_ context.Context, namespace string) ([]unstructured.Unstructured, error) {
	return c.contents[namespace], c.err
}

type stWorld struct {
	inventory          *stInventory
	management, remote client.Client
	clients            *stClients
	deps               Deps
	writes             []string
	recorder           *subTenantEventLog
}

func newSubTenantWorld(t *testing.T) *stWorld {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(naas.TenantGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "capsule.clastix.io", Version: "v1beta2", Kind: "GlobalTenantResource"}, &unstructured.Unstructured{})
	for _, kind := range []string{"PeerAuthentication", "AuthorizationPolicy"} {
		scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "security.istio.io", Version: "v1", Kind: kind}, &unstructured.Unstructured{})
	}
	registration := &cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "tenant-uid"}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{cloudstore.LabelTenant: "acme"}, Annotations: map[string]string{tenantlife.AnnotationRegistration: "tenant-uid"}}}
	parent := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "tenant-acme", UID: "parent-uid", Finalizers: []string{cloud.ClusterFinalizer}, Labels: map[string]string{cloudstore.LabelTenant: "acme", cloudstore.LabelID: stClusterID, cloudstore.LabelNetworkID: stNetworkID}}, Spec: cloud.ClusterSpec{NaaS: ptr.To(true)}, Status: cloud.ClusterStatus{Phase: cloud.ClusterPhaseRunning, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}, CurrentKubernetesVersion: "1.35", KubeconfigSecret: "cluster-kubeconfig", PlatformComponents: []cloud.PlatformComponentStatus{{Name: "capsule", Phase: "Ready"}, {Name: "karpenter", Phase: "Failed"}}}}
	sub := &cloud.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: stID, Namespace: "tenant-acme", UID: "subtenant-uid", Generation: 1}, Spec: cloud.SubTenantSpec{TenantRef: cloud.SubTenantTenantReference{Name: "acme", UID: "tenant-uid"}, ClusterRef: cloud.SubTenantClusterReference{ID: stClusterID, Name: "parent", UID: "parent-uid"}, DisplayName: "Payments", NamespaceKeys: []cloud.SubTenantNamespaceKey{"app"}, Quota: cloud.SubTenantQuota{NamespaceCount: 2, Hard: map[string]string{"requests.cpu": "500m", "requests.memory": "1Gi", "pods": "10"}}, PolicyProfile: "restricted-ambient-v1", DeletionProtection: ptr.To(false)}}
	management := fake.NewClientBuilder().WithScheme(scheme).WithObjects(registration, namespace, parent, sub).WithStatusSubresource(&cloud.SubTenant{}, &cloud.Cluster{}, &cloud.Tenant{}, &cloud.ReleaseHold{}).Build()
	bootstrap, err := naas.Load()
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{}
	for _, obj := range bootstrap.Objects() {
		obj.SetGeneration(1)
		if obj.GetKind() == "GlobalTenantResource" {
			obj.Object["status"] = map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "selectedTenants": []any{}}
		}
		objects = append(objects, obj)
	}
	remote := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	w := &stWorld{management: management, remote: remote, recorder: &subTenantEventLog{}}
	assertTenant := func(obj client.Object) error {
		if obj.GetObjectKind().GroupVersionKind() != naas.TenantGVK {
			return fmt.Errorf("controller attempted a forbidden workload write: %T %s", obj, obj.GetName())
		}
		return nil
	}
	audited := interceptor.NewClient(remote, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := assertTenant(obj); err != nil {
				return err
			}
			w.writes = append(w.writes, "create:"+obj.GetName())
			obj.SetUID(types.UID("remote-uid"))
			obj.SetGeneration(1)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := assertTenant(obj); err != nil {
				return err
			}
			w.writes = append(w.writes, "update:"+obj.GetName())
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := assertTenant(obj); err != nil {
				return err
			}
			w.writes = append(w.writes, "patch:"+obj.GetName())
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, namespace := obj.(*corev1.Namespace); namespace {
				options := &client.DeleteOptions{}
				for _, option := range opts {
					option.ApplyToDelete(options)
				}
				if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
					t.Fatal("namespace deletion lacked UID/RV protection")
				}
				w.writes = append(w.writes, "delete-namespace:"+obj.GetName())
				return c.Delete(ctx, obj, opts...)
			}
			if err := assertTenant(obj); err != nil {
				return err
			}
			w.writes = append(w.writes, "delete:"+obj.GetName())
			return c.Delete(ctx, obj, opts...)
		},
	})
	w.inventory = &stInventory{Client: audited, contents: map[string][]unstructured.Unstructured{}}
	w.clients = &stClients{client: w.inventory}
	w.deps = Deps{Direct: management, Deps: platform.Deps{Kube: management, KubeReader: management, SubTenantWorkloads: w.clients, Events: w.recorder}}
	return w
}
func (w *stWorld) pass(t *testing.T) {
	t.Helper()
	_, err := NewSubTenantReconciler(w.deps).Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "tenant-acme", Name: stID}})
	if err != nil {
		t.Fatal(err)
	}
}
func (w *stWorld) sub(t *testing.T) *cloud.SubTenant {
	t.Helper()
	obj := &cloud.SubTenant{}
	if err := w.management.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: stID}, obj); err != nil {
		t.Fatal(err)
	}
	return obj
}
func (w *stWorld) parent(t *testing.T) *cloud.Cluster {
	t.Helper()
	obj := &cloud.Cluster{}
	if err := w.management.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: "parent"}, obj); err != nil {
		t.Fatal(err)
	}
	return obj
}
func (w *stWorld) tenant(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(naas.TenantGVK)
	name, _ := naas.TenantName(stID)
	if err := w.remote.Get(t.Context(), client.ObjectKey{Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	return obj
}
func (w *stWorld) provision(t *testing.T) {
	t.Helper()
	w.pass(t)
	w.pass(t)
	if w.sub(t).Status.RemoteCapsuleTenantUID == "" {
		t.Fatal("remote identity not observed")
	}
}

func TestSubTenantControllerProtectsBeforeTenantOnlyWrites(t *testing.T) {
	w := newSubTenantWorld(t)
	w.pass(t)
	first := w.sub(t)
	if !slices.Contains(first.Finalizers, cloud.SubTenantFinalizer) || len(w.writes) != 0 {
		t.Fatal("remote write preceded optimistic finalizer acquisition")
	}
	w.pass(t)
	sub, remote := w.sub(t), w.tenant(t)
	if sub.Status.RemoteCapsuleTenantUID != "remote-uid" || sub.Status.TemplateRevision == "" || sub.Status.ObservedGeneration != 1 || meta.IsStatusConditionTrue(sub.Status.Conditions, "Ready") || meta.IsStatusConditionTrue(sub.Status.Conditions, "AccessReady") {
		t.Fatalf("false readiness or missing observations: %+v", sub.Status)
	}
	if remote.GetLabels()[naas.LabelAccessReady] != "false" || remote.GetLabels()[naas.LabelClusterUID] != "parent-uid" {
		t.Fatal(remote.GetLabels())
	}
	group, _, _ := unstructured.NestedString(remote.Object, "spec", "data", "naas", "developerGroup")
	pss, _, _ := unstructured.NestedString(remote.Object, "spec", "data", "naas", "pssVersion")
	if group != "" || pss != "v1.35" {
		t.Fatal("unresolved access or incorrect observed minor", group, pss)
	}
	rv := sub.ResourceVersion
	writes := len(w.writes)
	w.pass(t)
	if w.sub(t).ResourceVersion != rv || len(w.writes) != writes {
		t.Fatal("unchanged observations replayed status or workload writes")
	}
}

func TestSubTenantControllerRejectsForeignRemoteAndParentUID(t *testing.T) {
	for _, wrong := range []string{"remote ownership", "parent UID", "tenant UID"} {
		t.Run(wrong, func(t *testing.T) {
			w := newSubTenantWorld(t)
			switch wrong {
			case "remote ownership":
				b, _ := naas.Load()
				remote, err := naas.RenderTenant(w.sub(t), "1.35", b.Revision())
				if err != nil {
					t.Fatal(err)
				}
				remote.SetUID("foreign")
				labels := remote.GetLabels()
				labels[naas.LabelSubTenantUID] = "foreign"
				remote.SetLabels(labels)
				if err := w.remote.Create(t.Context(), remote); err != nil {
					t.Fatal(err)
				}
			case "parent UID":
				parent := w.parent(t)
				parent.UID = "replacement"
				if err := w.management.Update(t.Context(), parent); err != nil {
					t.Fatal(err)
				}
			case "tenant UID":
				tenant := &cloud.Tenant{}
				if err := w.management.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
					t.Fatal(err)
				}
				tenant.UID = "replacement"
				if err := w.management.Update(t.Context(), tenant); err != nil {
					t.Fatal(err)
				}
			}
			w.pass(t)
			if len(w.writes) != 0 || meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "Ready") {
				t.Fatal("foreign incarnation was adopted")
			}
		})
	}
	w := newSubTenantWorld(t)
	w.provision(t)
	remote := w.tenant(t)
	if err := w.remote.Delete(t.Context(), remote); err != nil {
		t.Fatal(err)
	}
	remote.SetResourceVersion("")
	remote.SetUID("replacement-remote")
	if err := w.remote.Create(t.Context(), remote); err != nil {
		t.Fatal(err)
	}
	writes := len(w.writes)
	w.pass(t)
	if len(w.writes) != writes || !strings.Contains(w.sub(t).Status.Message, "different recorded UID") {
		t.Fatal("last observed remote UID was not protected")
	}
}

func TestSubTenantControllerHonorsAllFreezeScopes(t *testing.T) {
	for _, scope := range []string{"platform", "tenant", "network", "cluster"} {
		t.Run(scope, func(t *testing.T) {
			w := newSubTenantWorld(t)
			hold := &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Scoped: scope != "platform", Reason: "maintenance", By: "operator"}}
			rule := cloud.FreezeRule{Frozen: true, Reason: "maintenance"}
			switch scope {
			case "tenant":
				rule.TenantID = "acme"
			case "network":
				rule.NetworkID = stNetworkID
			case "cluster":
				rule.ClusterID = stClusterID
			}
			if scope != "platform" {
				hold.Spec.Rules = []cloud.FreezeRule{rule}
			}
			if err := w.management.Create(t.Context(), hold); err != nil {
				t.Fatal(err)
			}
			w.pass(t)
			rv := w.sub(t).ResourceVersion
			w.pass(t)
			if w.clients.calls != 0 || len(w.writes) != 0 || !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, platform.ConditionFrozen) || w.sub(t).ResourceVersion != rv {
				t.Fatal("frozen allocation performed work or replayed observations")
			}
			if err := w.management.Delete(t.Context(), hold); err != nil {
				t.Fatal(err)
			}
			w.provision(t)
			if meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, platform.ConditionFrozen) {
				t.Fatal("resume retained stale freeze status")
			}
		})
	}
}

func TestSubTenantControllerPrerequisitesAndUpdateInvalidateAccess(t *testing.T) {
	w := newSubTenantWorld(t)
	bootstrap, _ := naas.Load()
	var missing *unstructured.Unstructured
	for _, obj := range bootstrap.Objects() {
		if obj.GetKind() == "GlobalTenantResource" {
			missing = obj
			break
		}
	}
	if err := w.remote.Delete(t.Context(), missing); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	if len(w.writes) != 0 || len(w.sub(t).Finalizers) != 0 || meta.FindStatusCondition(w.sub(t).Status.Conditions, "Ready").Reason != "BootstrapPending" {
		t.Fatal("missing bootstrap did not leave deletable pending intent")
	}
	if err := w.remote.Create(t.Context(), missing); err != nil {
		t.Fatal(err)
	}
	w.provision(t)
	sub := w.sub(t)
	sub.Spec.NamespaceKeys = append(sub.Spec.NamespaceKeys, "jobs")
	sub.Generation++
	if err := w.management.Update(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	remote := w.tenant(t)
	labels := remote.GetLabels()
	labels[naas.LabelAccessReady] = "true"
	remote.SetLabels(labels)
	if err := w.remote.Update(t.Context(), remote); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	if w.tenant(t).GetLabels()[naas.LabelAccessReady] != "false" {
		t.Fatal("spec update did not revoke gate first")
	}
	w.pass(t)
	keys, _, _ := unstructured.NestedStringSlice(w.tenant(t).Object, "spec", "data", "naas", "namespaceKeys")
	if len(keys) != 2 || w.sub(t).Status.ObservedGeneration != 2 || meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "Ready") {
		t.Fatal("new spec not reconciled conservatively")
	}
	// Current GTR content is checked, rather than trusting its Ready condition.
	gtr := missing.DeepCopy()
	if err := w.remote.Get(t.Context(), client.ObjectKeyFromObject(missing), gtr); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(gtr.Object, true, "spec", "cordoned"); err != nil {
		t.Fatal(err)
	}
	if err := w.remote.Update(t.Context(), gtr); err != nil {
		t.Fatal(err)
	}
	writes := len(w.writes)
	w.pass(t)
	if len(w.writes) != writes || w.sub(t).Status.TemplateRevision != "" {
		t.Fatal("stale template Ready bypassed current-revision check")
	}
}

func TestSubTenantControllerMissingClientAndIdentityResolver(t *testing.T) {
	w := newSubTenantWorld(t)
	w.deps.SubTenantWorkloads = nil
	w.pass(t)
	if len(w.writes) != 0 || len(w.sub(t).Finalizers) != 0 {
		t.Fatal("missing client acquired unsafe remote ownership")
	}
	w.deps.SubTenantWorkloads = w.clients
	sub := w.sub(t)
	sub.Spec.Access = &cloud.SubTenantAccess{Bindings: []cloud.SubTenantAccessBinding{{IdentityRef: "team", RoleProfile: "workload-editor"}}}
	if err := w.management.Update(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	if len(w.writes) != 0 || !strings.Contains(w.sub(t).Status.Message, "resolver") {
		t.Fatal("unresolved identity was granted access")
	}
}

func (w *stWorld) acknowledge(t *testing.T) {
	t.Helper()
	remote := w.tenant(t)
	remote.Object["status"] = map[string]any{"observedGeneration": remote.GetGeneration()}
	if err := w.remote.Update(t.Context(), remote); err != nil {
		t.Fatal(err)
	}
	for _, obj := range bootstrapObjects(t) {
		if obj.GetKind() != "GlobalTenantResource" {
			continue
		}
		if err := w.remote.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		obj.Object["status"] = map[string]any{"observedGeneration": obj.GetGeneration(), "selectedTenants": []any{}, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		if err := w.remote.Update(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubTenantControllerDeletionPreservesContentThenCleansEmptyNamespace(t *testing.T) {
	w := newDeletionWorld(t)
	w.provision(t)
	remote := w.tenant(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: remote.GetName() + "-app", UID: "namespace-uid", Labels: map[string]string{"capsule.clastix.io/tenant": remote.GetName(), naas.LabelSubTenantID: stID, naas.LabelTenant: "acme", "cloud.cluster-vending.io/naas-ready": "false", "projectcapsule.dev/cordoned": "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: naas.TenantGVK.GroupVersion().String(), Kind: "Tenant", Name: remote.GetName(), UID: remote.GetUID()}}}}
	if err := w.remote.Create(t.Context(), namespace); err != nil {
		t.Fatal(err)
	}
	pod := unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "app", "namespace": namespace.Name, "uid": "pod-uid"}}}
	w.inventory.contents[namespace.Name] = []unstructured.Unstructured{pod}
	if err := w.management.Delete(t.Context(), w.sub(t)); err != nil {
		t.Fatal(err)
	}
	parent := w.parent(t)
	parent.Spec.NaaS = ptr.To(false)
	if err := w.management.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	w.pass(t)
	if w.tenant(t).GetLabels()[naas.LabelProfile] != "terminating" {
		t.Fatal("generation not withdrawn before inventory")
	}
	before := len(w.writes)
	w.pass(t)
	if len(w.writes) != before {
		t.Fatal("stale acknowledgement permitted cleanup")
	}
	w.acknowledge(t)
	w.pass(t)
	if len(w.sub(t).Status.DeletionBlockers) != 1 || w.tenant(t).GetLabels()[naas.LabelProfile] != "terminating" {
		t.Fatal("content blocker lost or generation restored")
	}
	for _, write := range w.writes {
		if strings.HasPrefix(write, "delete") {
			t.Fatal("user content deleted", write)
		}
	}
	// The user removes their workload; management never deletes that Pod.
	w.inventory.contents[namespace.Name] = nil
	w.pass(t)
	w.acknowledge(t)
	w.pass(t)
	if !slices.Contains(w.writes, "delete-namespace:"+namespace.Name) {
		t.Fatal("empty namespace was not cleaned")
	}
	w.pass(t)
	w.acknowledge(t)
	w.pass(t)
	w.pass(t)
	gone := &cloud.SubTenant{}
	if err := w.management.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: stID}, gone); !apierrors.IsNotFound(err) {
		t.Fatal("cleanup did not release finalizer", err)
	}
}

func TestSubTenantControllerDeletionProtectionBlocksBeforeWorkloadCalls(t *testing.T) {
	w := newSubTenantWorld(t)
	w.provision(t)
	sub := w.sub(t)
	sub.Spec.DeletionProtection = nil
	if err := w.management.Update(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if err := w.management.Delete(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	calls, writes := w.clients.calls, len(w.writes)
	w.pass(t)
	w.pass(t)
	if w.clients.calls != calls || len(w.writes) != writes || !strings.Contains(w.sub(t).Status.Message, "protection") {
		t.Fatal("protected deletion reached workload API")
	}
}
func bootstrapObjects(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	b, err := naas.Load()
	if err != nil {
		t.Fatal(err)
	}
	return b.Objects()
}

type subTenantEventLog struct {
	reasons []string
	objects []platform.Object
}

func (s *subTenantEventLog) Event(_ context.Context, obj platform.Object, _ string, reason, _ string) {
	s.reasons = append(s.reasons, reason)
	s.objects = append(s.objects, obj)
}

func TestSubTenantControllerFreezeStopsUpdatesAndDeletion(t *testing.T) {
	w := newSubTenantWorld(t)
	w.provision(t)
	sub := w.sub(t)
	sub.Spec.NamespaceKeys = append(sub.Spec.NamespaceKeys, "jobs")
	sub.Generation++
	if err := w.management.Update(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	hold := &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Reason: "maintenance"}}
	if err := w.management.Create(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	writes, calls := len(w.writes), w.clients.calls
	w.pass(t)
	if err := w.management.Delete(t.Context(), w.sub(t)); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	if len(w.writes) != writes || w.clients.calls != calls || !slices.Contains(w.sub(t).Finalizers, cloud.SubTenantFinalizer) {
		t.Fatal("freeze allowed a workload update or destructive offboarding")
	}
}

func TestSubTenantControllerFinalizerConflictNeverWritesWorkload(t *testing.T) {
	w := newSubTenantWorld(t)
	w.deps.Direct = interceptor.NewClient(w.management.(client.WithWatch), interceptor.Funcs{Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
		return apierrors.NewConflict(schema.GroupResource{Group: cloud.SchemeGroupVersion.Group, Resource: "subtenants"}, obj.GetName(), fmt.Errorf("spec changed before finalizer claim"))
	}})
	_, err := NewSubTenantReconciler(w.deps).Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "tenant-acme", Name: stID}})
	if !apierrors.IsConflict(err) || len(w.writes) != 0 || len(w.sub(t).Finalizers) != 0 {
		t.Fatal("finalizer conflict did not prevent workload writes", err)
	}
}

func TestSubTenantControllerEventsArePersistedTransitions(t *testing.T) {
	w := newSubTenantWorld(t)
	w.provision(t)
	before := len(w.recorder.reasons)
	w.pass(t)
	w.pass(t)
	if len(w.recorder.reasons) != before {
		t.Fatal("unchanged observations replayed events")
	}
	if !slices.Contains(w.recorder.reasons, "ControllerProtectionAcquired") || !slices.Contains(w.recorder.reasons, "BaselinePending") {
		t.Fatal(w.recorder.reasons)
	}
	for _, obj := range w.recorder.objects {
		if obj.Kind != platform.KindSubTenant || obj.Namespace != "tenant-acme" || obj.Name != stID {
			t.Fatal("event attached to wrong resource", obj)
		}
	}
}
