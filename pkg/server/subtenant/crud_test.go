package subtenant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	tenantlife "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
)

const clusterID = "00000000-0000-4000-8000-000000000001"
const secondClusterID = "00000000-0000-4000-8000-000000000002"

type world struct {
	service *Service
	kube    client.Client
}

func newWorld(t *testing.T, funcs interceptor.Funcs) world {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := cloud.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var objects []client.Object
	for _, tenant := range []string{"acme", "other"} {
		uid := types.UID("tenant-" + tenant + "-1")
		objects = append(objects, &cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: tenant, UID: uid}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: kube.TenantNamespace(tenant), Labels: map[string]string{cloudstore.LabelTenant: tenant}, Annotations: map[string]string{tenantlife.AnnotationRegistration: string(uid)}}})
		for i, id := range []string{clusterID, secondClusterID} {
			objects = append(objects, &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cluster-%d", i), Namespace: kube.TenantNamespace(tenant), UID: types.UID(tenant + id), Finalizers: []string{cloud.ClusterFinalizer}, Labels: map[string]string{cloudstore.LabelTenant: tenant, cloudstore.LabelID: id}, Annotations: map[string]string{cloudstore.AnnotationRow: `{"etag":"before","state":"CLUSTER_STATE_RUNNING"}`}}, Spec: cloud.ClusterSpec{NaaS: ptr.To(true)}})
		}
	}
	create := funcs.Create
	funcs.Create = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		// The real API server assigns UIDs; fake clients do not.
		if child, ok := obj.(*cloud.SubTenant); ok && child.UID == "" {
			child.UID = types.UID(uuid.NewString())
		}
		if create != nil {
			return create(ctx, c, obj, opts...)
		}
		return c.Create(ctx, obj, opts...)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&cloud.SubTenant{}, &cloud.Cluster{}, &cloud.Tenant{}).WithInterceptorFuncs(funcs).Build()
	return world{service: NewService(reconcile.Deps{Kube: c}), kube: c}
}
func createRequest() *v1.CreateSubTenantRequest {
	// Lifecycle tests explicitly opt out; default protection is tested separately.
	return &v1.CreateSubTenantRequest{TenantId: "acme", ClusterId: clusterID, RequestId: uuid.NewString(), Subtenant: &v1.SubTenant{Spec: &v1.SubTenantSpec{Name: "allocation-" + uuid.NewString()[:12], DeletionProtection: ptr.To(false), DisplayName: "Payments", NamespaceKeys: []string{"app"}, Quota: &v1.SubTenantQuota{NamespaceCount: 2, Hard: map[string]string{"requests.cpu": "500m", "requests.memory": "1Gi", "pods": "10"}}, PolicyProfile: "restricted-ambient-v1"}}}
}
func (w world) create(t *testing.T) *v1.SubTenant {
	t.Helper()
	out, err := w.service.CreateSubTenant(t.Context(), createRequest())
	if err != nil {
		t.Fatal(err)
	}
	return out.Subtenant
}
func (w world) stored(t *testing.T, id string) *cloud.SubTenant {
	t.Helper()
	obj := &cloud.SubTenant{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: kube.TenantNamespace("acme"), Name: id}, obj); err != nil {
		t.Fatal(err)
	}
	return obj
}
func updateRequest(resource *v1.SubTenant, paths ...string) *v1.UpdateSubTenantRequest {
	return &v1.UpdateSubTenantRequest{TenantId: "acme", ClusterId: clusterID, Subtenant: &v1.SubTenant{Id: resource.Id, Etag: resource.Etag}, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}
}
func requireCode(t *testing.T, err error, code connect.Code) {
	t.Helper()
	if err == nil || connect.CodeOf(err) != code {
		t.Fatalf("error %v, want %s", err, code)
	}
}

func TestCRUDUsesOnlyKubernetesAndReportsPending(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	created := w.create(t)
	if created.Id == "" || created.Etag == "" || created.Generation != 1 || created.Status.State != v1.SubTenantState_SUB_TENANT_STATE_PENDING || created.Status.ObservedGeneration != 0 {
		t.Fatal(created)
	}
	obj := w.stored(t, created.Id)
	if obj.Spec.ClusterRef.ID != clusterID || obj.Spec.ClusterRef.Name != "cluster-0" || obj.Spec.ClusterRef.UID != "acme"+clusterID || obj.Spec.TenantRef.UID != "tenant-acme-1" || obj.Labels[crossplane.LabelManagedBy] != "cluster-vending" || len(obj.Finalizers) != 0 {
		t.Fatal(obj)
	}
	if obj.Status.State != "" {
		t.Fatal("CRUD wrote observed readiness")
	}
	got, err := w.service.GetSubTenant(t.Context(), &v1.GetSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id})
	if err != nil || got.Subtenant.Id != created.Id {
		t.Fatal(got, err)
	}
	req := updateRequest(created, "spec.display_name")
	req.Subtenant.Spec = &v1.SubTenantSpec{DisplayName: "Payments API"}
	updated, err := w.service.UpdateSubTenant(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Subtenant.Spec.DisplayName != "Payments API" || updated.Subtenant.Spec.Quota.Hard["requests.memory"] != "1Gi" || updated.Subtenant.Generation != 2 || updated.Subtenant.Etag == created.Etag || updated.Subtenant.Status.State != v1.SubTenantState_SUB_TENANT_STATE_PENDING {
		t.Fatal(updated)
	}
	requireCode(t, func() error { _, err := w.service.UpdateSubTenant(t.Context(), req); return err }(), connect.CodeAborted)
	namespaces := &corev1.NamespaceList{}
	if err := w.kube.List(t.Context(), namespaces); err != nil || len(namespaces.Items) != 2 {
		t.Fatal("CRUD created remote namespaces", err)
	}
}

func TestCreateIdempotencyBindsPayloadAndParent(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	req := createRequest()
	first, err := w.service.CreateSubTenant(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.service.CreateSubTenant(t.Context(), proto.CloneOf(req))
	if err != nil || first.Subtenant.Id != second.Subtenant.Id || first.Subtenant.Etag != second.Subtenant.Etag {
		t.Fatal(first, second, err)
	}
	obj := w.stored(t, first.Subtenant.Id)
	obj.Spec.DisplayName = "Updated later"
	if err := w.kube.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	replay, err := w.service.CreateSubTenant(t.Context(), req)
	if err != nil || replay.Subtenant.Spec.DisplayName != "Updated later" {
		t.Fatal("retry rewound desired state", replay, err)
	}
	changed := proto.CloneOf(req)
	changed.Subtenant.Spec.DisplayName = "Different"
	_, err = w.service.CreateSubTenant(t.Context(), changed)
	requireCode(t, err, connect.CodeAlreadyExists)
	changed = proto.CloneOf(req)
	changed.ClusterId = secondClusterID
	_, err = w.service.CreateSubTenant(t.Context(), changed)
	requireCode(t, err, connect.CodeAlreadyExists)
	list := &cloud.SubTenantList{}
	if err := w.kube.List(t.Context(), list); err != nil || len(list.Items) != 1 {
		t.Fatal("retry created a duplicate", err)
	}
	req.RequestId = ""
	_, err = w.service.CreateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeInvalidArgument)
}

func TestTenantAndClusterBoundaries(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	created := w.create(t)
	viewer := auth.WithIdentity(t.Context(), &auth.Identity{Subject: "viewer", Grants: []auth.Grant{{Role: auth.RoleTenantViewer, Tenants: []string{"acme"}}}})
	if _, err := w.service.GetSubTenant(viewer, &v1.GetSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id}); err != nil {
		t.Fatal(err)
	}
	_, err := w.service.CreateSubTenant(viewer, createRequest())
	requireCode(t, err, connect.CodePermissionDenied)
	_, err = w.service.ListSubTenants(viewer, &v1.ListSubTenantsRequest{TenantId: "other"})
	requireCode(t, err, connect.CodePermissionDenied)
	for _, req := range []*v1.GetSubTenantRequest{{TenantId: "other", ClusterId: clusterID, SubtenantId: created.Id}, {TenantId: "acme", ClusterId: secondClusterID, SubtenantId: created.Id}} {
		_, err := w.service.GetSubTenant(t.Context(), req)
		requireCode(t, err, connect.CodeNotFound)
	}
	req := updateRequest(created, "spec.display_name")
	req.Subtenant.ClusterId = secondClusterID
	req.Subtenant.Spec = &v1.SubTenantSpec{DisplayName: "escape"}
	_, err = w.service.UpdateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeInvalidArgument)
	tenant := &cloud.Tenant{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
		t.Fatal(err)
	}
	tenant.UID = "replacement-tenant"
	if err := w.kube.Update(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	_, err = w.service.GetSubTenant(t.Context(), &v1.GetSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id})
	requireCode(t, err, connect.CodeFailedPrecondition)
}

func TestUpdateMasksAppendOnlyAndIdentityEscalation(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	created := w.create(t)
	for _, paths := range [][]string{nil, {"status"}, {"cluster_id"}, {"spec"}, {"spec.access.bindings"}, {"spec.display_name", "spec.display_name"}} {
		_, err := w.service.UpdateSubTenant(t.Context(), updateRequest(created, paths...))
		requireCode(t, err, connect.CodeInvalidArgument)
	}
	req := updateRequest(created, "spec.namespace_keys")
	req.Subtenant.Spec = &v1.SubTenantSpec{NamespaceKeys: []string{"renamed"}}
	_, err := w.service.UpdateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeInvalidArgument)
	req.Subtenant.Spec.NamespaceKeys = []string{"app", "jobs"}
	updated, err := w.service.UpdateSubTenant(t.Context(), req)
	if err != nil || len(updated.Subtenant.Spec.NamespaceKeys) != 2 {
		t.Fatal(updated, err)
	}
	req = updateRequest(updated.Subtenant, "spec.policy_profile")
	req.Subtenant.Spec = &v1.SubTenantSpec{PolicyProfile: "privileged"}
	_, err = w.service.UpdateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeInvalidArgument)
	req = updateRequest(updated.Subtenant, "spec.access")
	req.Subtenant.Spec = &v1.SubTenantSpec{Access: &v1.SubTenantAccess{Bindings: []*v1.SubTenantAccessBinding{{IdentityRef: "approved-team", RoleProfile: "workload-editor"}}}}
	_, err = w.service.UpdateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeFailedPrecondition)
	req.Subtenant.Spec.Access.Bindings[0].RoleProfile = "cluster-admin"
	_, err = w.service.UpdateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeInvalidArgument)
	create := createRequest()
	create.Subtenant.Spec.ProtoReflect().SetUnknown([]byte{0xa2, 0x06, 0x03, 'x', 'y', 'z'})
	_, err = w.service.CreateSubTenant(t.Context(), create)
	requireCode(t, err, connect.CodeInvalidArgument)
}

func TestReadsAndDeletionWorkAfterParentDisabled(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	created := w.create(t)
	obj := w.stored(t, created.Id)
	// A workload controller must establish protection before provisioning.
	obj.Finalizers = []string{cloud.SubTenantFinalizer}
	if err := w.kube.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	obj.Status = cloud.SubTenantStatus{ObservedGeneration: 1, State: cloud.SubTenantStateReady, RemoteCapsuleTenant: "st-remote", Namespaces: []cloud.SubTenantNamespaceStatus{{Key: "app", Name: "st-remote-app", UID: "remote-uid"}}}
	if err := w.kube.Status().Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	parent := &cloud.Cluster{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: kube.TenantNamespace("acme"), Name: "cluster-0"}, parent); err != nil {
		t.Fatal(err)
	}
	parent.Spec.NaaS = ptr.To(false)
	if err := w.kube.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	_, err := w.service.CreateSubTenant(t.Context(), createRequest())
	requireCode(t, err, connect.CodeFailedPrecondition)
	got, err := w.service.GetSubTenant(t.Context(), &v1.GetSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.service.UpdateSubTenant(t.Context(), updateRequest(got.Subtenant, "spec.display_name"))
	requireCode(t, err, connect.CodeFailedPrecondition)
	listed, err := w.service.ListSubTenants(t.Context(), &v1.ListSubTenantsRequest{TenantId: "acme", ClusterId: clusterID})
	if err != nil || len(listed.Subtenants) != 1 {
		t.Fatal(listed, err)
	}
	_, err = w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id, Etag: created.Etag})
	requireCode(t, err, connect.CodeAborted)
	deleted, err := w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id, Etag: got.Subtenant.Etag})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Subtenant.Status.State != v1.SubTenantState_SUB_TENANT_STATE_DELETING || !strings.Contains(deleted.Subtenant.Status.Message, "until a controller") {
		t.Fatal(deleted)
	}
	current := w.stored(t, created.Id)
	if current.DeletionTimestamp.IsZero() || len(current.Finalizers) != 1 || current.Status.Namespaces[0].UID != "remote-uid" {
		t.Fatal("deletion removed observed data or protection")
	}
}

func TestKnownDeletionBlockersAndStaleReady(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	created := w.create(t)
	obj := w.stored(t, created.Id)
	obj.Status = cloud.SubTenantStatus{State: cloud.SubTenantStateReady, ObservedGeneration: 0, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Old", Message: "old qualification"}}, DeletionBlockers: []string{"workloads still exist"}}
	if err := w.kube.Status().Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	got, err := w.service.GetSubTenant(t.Context(), &v1.GetSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Subtenant.Status.State != v1.SubTenantState_SUB_TENANT_STATE_PENDING || got.Subtenant.Status.Conditions[0].Status != v1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Fatal("stale status claimed Ready")
	}
	_, err = w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id, Etag: got.Subtenant.Etag})
	requireCode(t, err, connect.CodeFailedPrecondition)
	if !w.stored(t, created.Id).DeletionTimestamp.IsZero() {
		t.Fatal("known workload blocker was ignored")
	}
}

func TestListFiltersParentInKubernetesAndScopesCursors(t *testing.T) {
	selectors := []string{}
	w := newWorld(t, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*cloud.SubTenantList); ok {
			options := &client.ListOptions{}
			for _, opt := range opts {
				opt.ApplyToList(options)
			}
			selectors = append(selectors, options.LabelSelector.String())
		}
		return c.List(ctx, list, opts...)
	}})
	for range 3 {
		w.create(t)
	}
	req := createRequest()
	req.ClusterId = secondClusterID
	if _, err := w.service.CreateSubTenant(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	token := ""
	for {
		page, err := w.service.ListSubTenants(t.Context(), &v1.ListSubTenantsRequest{TenantId: "acme", ClusterId: clusterID, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Subtenants {
			if seen[item.Id] || item.ClusterId != clusterID {
				t.Fatal("duplicated or foreign item")
			}
			seen[item.Id] = true
		}
		token = page.NextPageToken
		if token == "" {
			break
		}
		_, err = w.service.ListSubTenants(t.Context(), &v1.ListSubTenantsRequest{TenantId: "other", ClusterId: clusterID, PageToken: token})
		requireCode(t, err, connect.CodeInvalidArgument)
	}
	if len(seen) != 3 {
		t.Fatal("list omitted pages")
	}
	for _, selector := range selectors {
		if !strings.Contains(selector, cloudstore.LabelClusterID+"="+clusterID) {
			t.Fatal("parent filter not sent to Kubernetes", selector)
		}
	}
	all, err := w.service.ListSubTenants(t.Context(), &v1.ListSubTenantsRequest{TenantId: "acme"})
	if err != nil || len(all.Subtenants) != 4 {
		t.Fatal(all, err)
	}
}

func TestCreateParentRaceRollsBackWithoutDroppingFinalizer(t *testing.T) {
	for _, change := range []string{"disabled", "deleting", "replaced UID"} {
		t.Run(change, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*cloud.SubTenant); ok {
					parent := &cloud.Cluster{}
					if err := c.Get(ctx, client.ObjectKey{Namespace: kube.TenantNamespace("acme"), Name: "cluster-0"}, parent); err != nil {
						return err
					}
					switch change {
					case "disabled":
						parent.Spec.NaaS = ptr.To(false)
						if err := c.Update(ctx, parent); err != nil {
							return err
						}
					case "deleting":
						if err := c.Delete(ctx, parent); err != nil {
							return err
						}
					case "replaced UID":
						parent.UID = "replacement-cluster"
						if err := c.Update(ctx, parent); err != nil {
							return err
						}
					}
				}
				return c.Create(ctx, obj, opts...)
			}})
			_, err := w.service.CreateSubTenant(t.Context(), createRequest())
			requireCode(t, err, connect.CodeAborted)
			allocations := &cloud.SubTenantList{}
			if err := w.kube.List(t.Context(), allocations); err != nil {
				t.Fatal(err)
			}
			if len(allocations.Items) != 0 {
				t.Fatal("race rollback retained unprovisioned intent")
			}
		})
	}
}

func TestClaimParentUsesOptimisticLockAndBumpsStoredEtag(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	w.create(t)
	parent := &cloud.Cluster{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: kube.TenantNamespace("acme"), Name: "cluster-0"}, parent); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if err := json.Unmarshal([]byte(parent.Annotations[cloudstore.AnnotationRow]), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["etag"] == "before" || metadata["state"] != "CLUSTER_STATE_RUNNING" {
		t.Fatal("parent claim failed to invalidate stale API writes", metadata)
	}
	conflicted := newWorld(t, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*cloud.Cluster); ok {
			return apierrors.NewConflict(schema.GroupResource{Group: cloud.SchemeGroupVersion.Group, Resource: "clusters"}, obj.GetName(), fmt.Errorf("concurrent parent mutation"))
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	_, err := conflicted.service.CreateSubTenant(t.Context(), createRequest())
	requireCode(t, err, connect.CodeAborted)
	allocations := &cloud.SubTenantList{}
	if err := conflicted.kube.List(t.Context(), allocations); err != nil || len(allocations.Items) != 0 {
		t.Fatal("child created without claiming parent", err)
	}
}

func TestStrictParentPreconditionsAndImmutableIncarnation(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	created := w.create(t)
	parent := &cloud.Cluster{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: kube.TenantNamespace("acme"), Name: "cluster-0"}, parent); err != nil {
		t.Fatal(err)
	}
	parent.UID = "new-cluster-uid"
	if err := w.kube.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	req := updateRequest(created, "spec.display_name")
	req.Subtenant.Spec = &v1.SubTenantSpec{DisplayName: "No adoption"}
	_, err := w.service.UpdateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeFailedPrecondition)
	parent.Annotations[cloudstore.AnnotationRow] = `{"etag":"deleting","state":"CLUSTER_STATE_DELETING"}`
	if err := w.kube.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	_, err = w.service.CreateSubTenant(t.Context(), createRequest())
	requireCode(t, err, connect.CodeFailedPrecondition)
	if _, err := w.service.GetSubTenant(t.Context(), &v1.GetSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id}); err != nil {
		t.Fatal("read should survive a deleting parent", err)
	}
}

func TestPostCreateClaimInvalidatesParentWritePreparedBeforeChild(t *testing.T) {
	var prepared *cloud.Cluster
	w := newWorld(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*cloud.SubTenant); ok {
			prepared = &cloud.Cluster{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: kube.TenantNamespace("acme"), Name: "cluster-0"}, prepared); err != nil {
				return err
			}
		}
		return c.Create(ctx, obj, opts...)
	}})
	w.create(t)
	if prepared == nil {
		t.Fatal("did not capture a parent transaction")
	}
	prepared.Spec.NaaS = ptr.To(false)
	if err := w.kube.Update(t.Context(), prepared); !apierrors.IsConflict(err) {
		t.Fatalf("old transaction could disable NaaS after child creation: %v", err)
	}
}

func TestPostCreateClaimRechecksDisabledParentAfterConflict(t *testing.T) {
	calls := 0
	w := newWorld(t, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*cloud.Cluster); ok {
			calls++
			if calls == 2 {
				parent := &cloud.Cluster{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), parent); err != nil {
					return err
				}
				parent.Spec.NaaS = ptr.To(false)
				if err := c.Update(ctx, parent); err != nil {
					return err
				}
			}
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	_, err := w.service.CreateSubTenant(t.Context(), createRequest())
	requireCode(t, err, connect.CodeAborted)
	objects := &cloud.SubTenantList{}
	if err := w.kube.List(t.Context(), objects); err != nil {
		t.Fatal(err)
	}
	if len(objects.Items) != 0 {
		t.Fatal("parent claim retried without checking disabled NaaS or retained unprovisioned intent")
	}
}

func TestConcurrentCreateRetriesConvergeToOneAllocation(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	request := createRequest()
	type result struct {
		resource *v1.CreateSubTenantResponse
		err      error
	}
	results := make(chan result, 4)
	for range 4 {
		go func() {
			resource, err := w.service.CreateSubTenant(t.Context(), proto.CloneOf(request))
			results <- result{resource, err}
		}()
	}
	id := ""
	for range 4 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if id == "" {
			id = result.resource.Subtenant.Id
		} else if result.resource.Subtenant.Id != id {
			t.Fatal("retry allocated another identity")
		}
	}
	objects := &cloud.SubTenantList{}
	if err := w.kube.List(t.Context(), objects); err != nil {
		t.Fatal(err)
	}
	if len(objects.Items) != 1 || !objects.Items[0].DeletionTimestamp.IsZero() {
		t.Fatal("concurrent retries duplicated or rolled back a valid allocation")
	}
}

func TestUnprovisionedIntentCanBeDeletedWithoutController(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	created := w.create(t)
	if len(w.stored(t, created.Id).Finalizers) != 0 {
		t.Fatal("CRUD must not acquire controller ownership")
	}
	result, err := w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id, Etag: created.Etag})
	if err != nil {
		t.Fatal(err)
	}
	if result.Subtenant.Id != created.Id || !strings.Contains(result.Subtenant.Status.Message, "intent was removed") {
		t.Fatal(result)
	}
	_, err = w.service.GetSubTenant(t.Context(), &v1.GetSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id})
	requireCode(t, err, connect.CodeNotFound)
}

func TestDeleteFailsClosedForUnprotectedRemoteState(t *testing.T) {
	for _, status := range []cloud.SubTenantStatus{
		{RemoteCapsuleTenant: "st-created"},
		{State: cloud.SubTenantStateProvisioning},
		{Namespaces: []cloud.SubTenantNamespaceStatus{{Key: "app", Name: "remote-app"}}},
		{TemplateRevision: "qualified-template"},
	} {
		w := newWorld(t, interceptor.Funcs{})
		created := w.create(t)
		obj := w.stored(t, created.Id)
		obj.Status = status
		if err := w.kube.Status().Update(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
		_, err := w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id, Etag: obj.ResourceVersion})
		requireCode(t, err, connect.CodeFailedPrecondition)
		if !w.stored(t, created.Id).DeletionTimestamp.IsZero() {
			t.Fatal("unprotected remote data allowed deletion")
		}
	}
}

func TestControllerFinalizerRaceAbortsIntentDeletion(t *testing.T) {
	raced := false
	w := newWorld(t, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*cloud.SubTenant); ok && !raced {
			raced = true
			controller := &cloud.SubTenant{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), controller); err != nil {
				return err
			}
			controller.Finalizers = append(controller.Finalizers, cloud.SubTenantFinalizer)
			if err := c.Update(ctx, controller); err != nil {
				return err
			}
		}
		return c.Delete(ctx, obj, opts...)
	}})
	created := w.create(t)
	_, err := w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id, Etag: created.Etag})
	requireCode(t, err, connect.CodeAborted)
	obj := w.stored(t, created.Id)
	if len(obj.Finalizers) != 1 || !obj.DeletionTimestamp.IsZero() {
		t.Fatal("deletion ignored the controller's optimistic ownership claim")
	}
	protected, err := w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: created.Id, Etag: obj.ResourceVersion})
	if err != nil || protected.Subtenant.Status.State != v1.SubTenantState_SUB_TENANT_STATE_DELETING {
		t.Fatal(protected, err)
	}
	if len(w.stored(t, created.Id).Finalizers) != 1 {
		t.Fatal("API stripped the controller finalizer")
	}
}

func TestCreateRollbackRetainsConcurrentControllerProtection(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		if child, ok := obj.(*cloud.SubTenant); ok {
			child.Finalizers = append(child.Finalizers, cloud.SubTenantFinalizer)
			if err := c.Update(ctx, child); err != nil {
				return err
			}
			parent := &cloud.Cluster{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: child.Namespace, Name: child.Spec.ClusterRef.Name}, parent); err != nil {
				return err
			}
			parent.Spec.NaaS = ptr.To(false)
			if err := c.Update(ctx, parent); err != nil {
				return err
			}
		}
		return nil
	}})
	_, err := w.service.CreateSubTenant(t.Context(), createRequest())
	requireCode(t, err, connect.CodeAborted)
	objects := &cloud.SubTenantList{}
	if err := w.kube.List(t.Context(), objects); err != nil {
		t.Fatal(err)
	}
	if len(objects.Items) != 1 || objects.Items[0].DeletionTimestamp.IsZero() || len(objects.Items[0].Finalizers) != 1 {
		t.Fatal("rollback removed protection acquired by a controller")
	}
}
