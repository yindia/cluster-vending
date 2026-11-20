package subtenant

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestCanonicalNameValidationAndImmutability(t *testing.T) {
	for _, name := range []string{"", "Uppercase", "a/b", "a-", "-a", strings.Repeat("a", 36)} {
		w := newWorld(t, interceptor.Funcs{})
		req := createRequest()
		req.Subtenant.Spec.Name = name
		_, err := w.service.CreateSubTenant(t.Context(), req)
		requireCode(t, err, connect.CodeInvalidArgument)
	}
	w := newWorld(t, interceptor.Funcs{})
	req := createRequest()
	req.Subtenant.Spec.Name = strings.Repeat("a", 35)
	created, err := w.service.CreateSubTenant(t.Context(), req)
	if err != nil || created.Subtenant.Spec.Name != req.Subtenant.Spec.Name {
		t.Fatal(created, err)
	}
	update := updateRequest(created.Subtenant, "spec.name")
	update.Subtenant.Spec = &v1.SubTenantSpec{Name: "renamed"}
	_, err = w.service.UpdateSubTenant(t.Context(), update)
	requireCode(t, err, connect.CodeInvalidArgument)
	legacy := w.stored(t, created.Subtenant.Id)
	legacy.Spec.Name = ""
	legacy.Spec.DeletionProtection = nil
	if err := w.kube.Update(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	update = updateRequest(resourceView(legacy), "spec.display_name")
	update.Subtenant.Spec = &v1.SubTenantSpec{DisplayName: "Legacy still editable"}
	updated, err := w.service.UpdateSubTenant(t.Context(), update)
	if err != nil || updated.Subtenant.Spec.Name != "" || !updated.Subtenant.Spec.GetDeletionProtection() {
		t.Fatal(updated, err)
	}
}

func TestDeletionProtectionDefaultsAndExplicitDisable(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	req := createRequest()
	req.Subtenant.Spec.DeletionProtection = nil
	created, err := w.service.CreateSubTenant(t.Context(), req)
	if err != nil || !created.GetSubtenant().GetSpec().GetDeletionProtection() {
		t.Fatal(created, err)
	}
	resource := created.Subtenant
	_, err = w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: resource.Id, Etag: resource.Etag})
	requireCode(t, err, connect.CodeFailedPrecondition)
	if !w.stored(t, resource.Id).DeletionTimestamp.IsZero() {
		t.Fatal("protected delete changed allocation")
	}
	update := updateRequest(resource, "spec.deletion_protection")
	update.Subtenant.Spec = &v1.SubTenantSpec{}
	_, err = w.service.UpdateSubTenant(t.Context(), update)
	requireCode(t, err, connect.CodeInvalidArgument)
	update.Subtenant.Spec.DeletionProtection = ptr.To(false)
	unprotected, err := w.service.UpdateSubTenant(t.Context(), update)
	if err != nil || unprotected.Subtenant.Spec.DeletionProtection == nil || unprotected.Subtenant.Spec.GetDeletionProtection() {
		t.Fatal(unprotected, err)
	}
	_, err = w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: resource.Id, Etag: unprotected.Subtenant.Etag})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeletingAllocationOnlyAllowsExplicitProtectionDisable(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	resource := w.create(t)
	obj := w.stored(t, resource.Id)
	obj.Spec.DeletionProtection = nil // Legacy absence must also remain protected.
	obj.Finalizers = []string{cloud.SubTenantFinalizer}
	if err := w.kube.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if err := w.kube.Delete(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	obj = w.stored(t, resource.Id)
	for _, paths := range [][]string{{"spec.display_name"}, {"spec.deletion_protection", "spec.display_name"}, {"spec.deletion_protection"}} {
		update := updateRequest(resourceView(obj), paths...)
		update.Subtenant.Spec = &v1.SubTenantSpec{DisplayName: "must not change", DeletionProtection: ptr.To(true)}
		_, err := w.service.UpdateSubTenant(t.Context(), update)
		requireCode(t, err, connect.CodeFailedPrecondition)
	}
	parent := &cloud.Cluster{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: obj.Namespace, Name: obj.Spec.ClusterRef.Name}, parent); err != nil {
		t.Fatal(err)
	}
	parent.Spec.NaaS = ptr.To(false)
	if err := w.kube.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	update := updateRequest(resourceView(obj), "spec.deletion_protection")
	update.Subtenant.Spec = &v1.SubTenantSpec{DeletionProtection: ptr.To(false), DisplayName: "ignored unmasked value"}
	updated, err := w.service.UpdateSubTenant(t.Context(), update)
	if err != nil || updated.Subtenant.Spec.GetDeletionProtection() || updated.Subtenant.Spec.DisplayName != resource.Spec.DisplayName {
		t.Fatal(updated, err)
	}
	if stored := w.stored(t, resource.Id); stored.DeletionTimestamp.IsZero() || len(stored.Finalizers) != 1 {
		t.Fatal("unprotect modified lifecycle ownership")
	}
}

func TestConcurrentCanonicalNameClaimHasOneOwner(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	results := make(chan error, 6)
	for range 6 {
		go func() {
			req := createRequest()
			req.Subtenant.Spec.Name = "payments"
			_, err := w.service.CreateSubTenant(t.Context(), req)
			results <- err
		}()
	}
	success := 0
	for range 6 {
		if err := <-results; err == nil {
			success++
		} else if connect.CodeOf(err) != connect.CodeAlreadyExists {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("%d concurrent allocations own one name", success)
	}
	allocations := &cloud.SubTenantList{}
	if err := w.kube.List(t.Context(), allocations); err != nil || len(allocations.Items) != 1 {
		t.Fatal(allocations, err)
	}
	obj := &allocations.Items[0]
	claim := &corev1.ConfigMap{}
	if err := w.kube.Get(t.Context(), nameClaimKey(obj.Namespace, obj.Spec.ClusterRef.UID, "payments"), claim); err != nil {
		t.Fatal(err)
	}
	if claim.Data["allocationID"] != obj.Name || len(claim.OwnerReferences) != 1 || claim.OwnerReferences[0].UID != obj.UID {
		t.Fatal("claim not bound to persisted allocation UID", claim)
	}
	other := createRequest()
	other.ClusterId = secondClusterID
	other.Subtenant.Spec.Name = "payments"
	if _, err := w.service.CreateSubTenant(t.Context(), other); err != nil {
		t.Fatal("different cluster cannot reuse canonical name", err)
	}
}

func TestNameClaimCrashRecoveryAndRetention(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	w := newWorld(t, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*cloud.SubTenant); ok && fail.Swap(false) {
			return fmt.Errorf("simulated process failure after name reservation")
		}
		return c.Create(ctx, obj, opts...)
	}})
	req := createRequest()
	req.Subtenant.Spec.Name = "payments"
	_, err := w.service.CreateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeUnavailable)
	competitor := proto.CloneOf(req)
	competitor.RequestId = uuid.NewString()
	_, err = w.service.CreateSubTenant(t.Context(), competitor)
	requireCode(t, err, connect.CodeAlreadyExists)
	created, err := w.service.CreateSubTenant(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	obj := w.stored(t, created.Subtenant.Id)
	claim := &corev1.ConfigMap{}
	key := nameClaimKey(obj.Namespace, obj.Spec.ClusterRef.UID, obj.Spec.Name)
	if err := w.kube.Get(t.Context(), key, claim); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after CR persistence but before binding its claim.
	claim.OwnerReferences = nil
	if err := w.kube.Update(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if _, err := w.service.CreateSubTenant(t.Context(), req); err != nil {
		t.Fatal("same-request retry could not recover ownerless claim", err)
	}
	obj.Finalizers = []string{cloud.SubTenantFinalizer}
	if err := w.kube.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	_, err = w.service.DeleteSubTenant(t.Context(), &v1.DeleteSubTenantRequest{TenantId: "acme", ClusterId: clusterID, SubtenantId: obj.Name, Etag: obj.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.service.CreateSubTenant(t.Context(), competitor)
	requireCode(t, err, connect.CodeAlreadyExists)
	if err := w.kube.Get(t.Context(), key, claim); err != nil || len(claim.OwnerReferences) != 1 {
		t.Fatal("claim released while owner still terminating", claim, err)
	}
}

func TestCreateAndReplayWithUncachedNameReservations(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	w.service.deps.KubeReader = w.kube
	failBind := true
	w.service.deps.Kube = interceptor.NewClient(w.kube.(client.WithWatch), interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			t.Fatalf("ownership decision used a cached read of %T", obj)
			return nil
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, claim := obj.(*corev1.ConfigMap); claim && failBind {
				failBind = false
				return fmt.Errorf("simulated interruption binding name claim")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	req := createRequest()
	_, err := w.service.CreateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeUnavailable)
	if !strings.Contains(err.Error(), "was recorded") {
		t.Fatalf("create did not report the persisted allocation: %v", err)
	}
	id, _, err := createIdentity(req)
	if err != nil {
		t.Fatal(err)
	}
	allocation := w.stored(t, id)
	for range 2 { // First repair the missing owner, then verify the bound reservation.
		out, err := w.service.CreateSubTenant(t.Context(), req)
		if err != nil || out.GetSubtenant().GetId() != id {
			t.Fatalf("replaying persisted request: %v, %v", out, err)
		}
	}
	claim := &corev1.ConfigMap{}
	if err := w.kube.Get(t.Context(), nameClaimKey(allocation.Namespace, allocation.Spec.ClusterRef.UID, req.Subtenant.Spec.Name), claim); err != nil {
		t.Fatal(err)
	}
	if len(claim.OwnerReferences) != 1 || claim.OwnerReferences[0].UID != allocation.UID {
		t.Fatalf("expected allocation UID %s to own the reservation, got %v", allocation.UID, claim.OwnerReferences)
	}
	req.RequestId = uuid.NewString()
	_, err = w.service.CreateSubTenant(t.Context(), req)
	requireCode(t, err, connect.CodeAlreadyExists)
}

func TestGroupsPersistAsExactClaimsBeforeOIDCReady(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{})
	req := createRequest()
	req.Subtenant.Spec.Access = &v1.SubTenantAccess{AdminGroups: []string{"oidc:payments-admins"}, ViewerGroups: []string{"finance viewers"}}
	created, err := w.service.CreateSubTenant(t.Context(), req)
	if err != nil || !proto.Equal(created.GetSubtenant().GetSpec().GetAccess(), req.Subtenant.Spec.Access) {
		t.Fatal(created, err)
	}
	stored := w.stored(t, created.Subtenant.Id)
	if stored.Spec.Access.AdminGroups[0] != "oidc:payments-admins" || created.Subtenant.Status.State != v1.SubTenantState_SUB_TENANT_STATE_PENDING {
		t.Fatal("group intent was lost or readiness fabricated")
	}
	update := updateRequest(created.Subtenant, "spec.access")
	update.Subtenant.Spec = &v1.SubTenantSpec{Access: &v1.SubTenantAccess{ViewerGroups: []string{"new-viewers"}}}
	updated, err := w.service.UpdateSubTenant(t.Context(), update)
	if err != nil || len(updated.GetSubtenant().GetSpec().GetAccess().GetAdminGroups()) != 0 || updated.Subtenant.Spec.Access.ViewerGroups[0] != "new-viewers" {
		t.Fatal(updated, err)
	}
	// Conversion must clone slices, not share mutable API/native state.
	apiSpec := specFromCR(stored.Spec)
	apiSpec.Access.AdminGroups[0] = "changed"
	if stored.Spec.Access.AdminGroups[0] == "changed" {
		t.Fatal("native to API group conversion aliases stored slices")
	}
	native := specToCR(req.Subtenant.Spec)
	req.Subtenant.Spec.Access.ViewerGroups[0] = "changed"
	if native.Access.ViewerGroups[0] == "changed" {
		t.Fatal("API to native group conversion aliases request slices")
	}
}

func TestSubTenantJSONPreservesFalseProtectionAndExactGroups(t *testing.T) {
	want := &v1.SubTenantSpec{Name: "payments", DeletionProtection: ptr.To(false), Access: &v1.SubTenantAccess{AdminGroups: []string{"oidc:admins"}, ViewerGroups: []string{"finance viewers"}}}
	raw, err := protojson.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got := &v1.SubTenantSpec{}
	if err := protojson.Unmarshal(raw, got); err != nil || !proto.Equal(want, got) || got.DeletionProtection == nil {
		t.Fatalf("JSON lost explicit false or group values: %s, %v", raw, err)
	}
	if !strings.Contains(string(raw), `"deletionProtection":false`) {
		t.Fatal("explicit false omitted from JSON", string(raw))
	}
}

func TestGroupValidationRejectsReservedWildcardAndOverlappingClaims(t *testing.T) {
	for _, access := range []*v1.SubTenantAccess{
		{AdminGroups: []string{"system:masters"}}, {ViewerGroups: []string{"team-*"}}, {ViewerGroups: []string{"team-?"}},
		{AdminGroups: []string{"group", "group"}}, {AdminGroups: []string{"group"}, ViewerGroups: []string{"group"}},
		{AdminGroups: []string{""}}, {ViewerGroups: []string{" group "}}, {AdminGroups: []string{"a\nb"}},
		{ViewerGroups: []string{strings.Repeat("a", 257)}},
	} {
		w := newWorld(t, interceptor.Funcs{})
		req := createRequest()
		req.Subtenant.Spec.Access = access
		_, err := w.service.CreateSubTenant(t.Context(), req)
		requireCode(t, err, connect.CodeInvalidArgument)
	}
	groups := make([]string, 33)
	for i := range groups {
		groups[i] = fmt.Sprintf("g-%d", i)
	}
	if validateGroups(groups, nil) == nil {
		t.Fatal("accepted too many groups")
	}
	if err := validateGroups([]string{strings.Repeat("a", 256)}, nil); err != nil {
		t.Fatal("rejected maximum-length exact group", err)
	}
}
