package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// Managed-object reads may use the ordinary fake. Only SubTenant lists come
// from a separate API reader, exposing children absent from the cache.
type subTenantSafetyReader struct {
	client.Reader
	direct client.Client
	calls  int
	err    error
}

func (r *subTenantSafetyReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*cloudv1alpha1.SubTenantList); !ok {
		return r.Reader.List(ctx, list, opts...)
	}
	r.calls++
	if r.err != nil {
		return r.err
	}
	return r.direct.List(ctx, list, opts...)
}

func subTenantSafetyWorld(t *testing.T) (world, *subTenantSafetyReader) {
	t.Helper()
	w := newWorld(t, interceptor.Funcs{}, nil)
	// Do not register this type ad hoc in the fake: production kube.Scheme
	// must know it, or a missing-kind path could make a safety test vacuous.
	gvk := cloudv1alpha1.SchemeGroupVersion.WithKind("SubTenantList")
	if !w.kube.Scheme().Recognizes(gvk) {
		t.Fatalf("production scheme does not register %s", gvk)
	}
	direct := fake.NewClientBuilder().WithScheme(w.kube.Scheme()).Build()
	reader := &subTenantSafetyReader{Reader: w.kube, direct: direct}
	deps := w.svc.deps
	deps.KubeReader = reader
	w.svc = NewClusterService(deps)
	return w, reader
}

func addSubTenantDependency(t *testing.T, reader *subTenantSafetyReader, name, namespace, clusterID string, labels map[string]string, terminating bool) {
	t.Helper()
	child := &cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
	child.Spec.ClusterRef.ID = clusterID
	if terminating {
		child.Finalizers = []string{"test.cluster-vending.io/cleanup"}
	}
	if err := reader.direct.Create(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	if terminating {
		if err := reader.direct.Delete(t.Context(), child); err != nil {
			t.Fatal(err)
		}
		if err := reader.direct.Get(t.Context(), client.ObjectKeyFromObject(child), child); err != nil {
			t.Fatal(err)
		}
		if child.DeletionTimestamp.IsZero() {
			t.Fatal("test child is not actually terminating")
		}
	}
}

func TestSubTenantSafetyBlocksClusterDeletionIncludingForce(t *testing.T) {
	for _, test := range []struct {
		name        string
		terminating bool
		label       string
		nameLabel   bool
	}{
		{name: "unlabeled spec reference"},
		{name: "terminating spec reference", terminating: true},
		{name: "cluster ID label", label: cloudstore.LabelClusterID},
		{name: "cluster reference ID label", label: cloudv1alpha1.LabelCluster},
		{name: "cluster name label", label: cloudv1alpha1.LabelCluster, nameLabel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, reader := subTenantSafetyWorld(t)
			clusterID := w.row.ID
			labels := map[string]string{}
			if test.label != "" {
				clusterID = "another-cluster"
				labels[test.label] = w.row.ID
				if test.nameLabel {
					labels[test.label] = w.row.Name
				}
			}
			addSubTenantDependency(t, reader, "child", worldNS, clusterID, labels, test.terminating)
			before := w.cluster(t)
			for _, force := range []bool{false, true} {
				_, err := w.svc.DeleteCluster(t.Context(), &cloudv1.DeleteClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID, Force: force})
				if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "child") {
					t.Fatalf("force=%t: got %v, want FAILED_PRECONDITION identifying child", force, err)
				}
				after := w.cluster(t)
				if after.State != before.State || after.DeleteForce != before.DeleteForce || after.ETag != before.ETag {
					t.Fatal("rejected deletion mutated the stored parent")
				}
			}
			if reader.calls != 2 {
				t.Fatalf("fresh SubTenant lists = %d, want 2", reader.calls)
			}
		})
	}
}

func TestSubTenantSafetyUIDOnlyDependencyBlocksRPCs(t *testing.T) {
	w, reader := subTenantSafetyWorld(t)
	parent := &cloudv1alpha1.Cluster{}
	if err := w.storeKube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: w.row.Name}, parent); err != nil {
		t.Fatal(err)
	}
	// Fake Kubernetes does not allocate UIDs. Give the management CR its
	// immutable identity explicitly; the RPC must resolve it via its reader.
	parent.UID = types.UID("rpc-parent-uid")
	if err := w.storeKube.Update(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	reader.Reader = w.storeKube
	child := &cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: "uid-only-child", Namespace: worldNS}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: cloudv1alpha1.SubTenantClusterReference{UID: string(parent.UID)}}}
	if err := reader.direct.Create(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	_, err := w.svc.DeleteCluster(t.Context(), &cloudv1.DeleteClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), child.Name) {
		t.Fatalf("UID-only deletion dependency missed: %v", err)
	}
	req := updateRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(false)}, "spec.naas")
	req.ValidateOnly = true
	if _, err := w.svc.UpdateCluster(t.Context(), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("UID-only validate-only disable dependency missed: %v", err)
	}
}

func TestSubTenantSafetyScopesDependenciesAndAllowsDeletionAfterCleanup(t *testing.T) {
	w, reader := subTenantSafetyWorld(t)
	addSubTenantDependency(t, reader, "foreign-namespace", "tenant-other", w.row.ID, nil, false)
	addSubTenantDependency(t, reader, "other-cluster", worldNS, "other-cluster", nil, false)
	addSubTenantDependency(t, reader, "child", worldNS, w.row.ID, nil, false)
	if _, err := w.svc.DeleteCluster(t.Context(), &cloudv1.DeleteClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("live child did not block: %v", err)
	}
	if err := reader.direct.Delete(t.Context(), &cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: worldNS}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.DeleteCluster(t.Context(), &cloudv1.DeleteClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID}); err != nil {
		t.Fatal(err)
	}
	if w.cluster(t).State != reconcilecluster.StateDeleting {
		t.Fatal("parent was not scheduled for deletion after cleanup")
	}
}

func TestSubTenantSafetyBlocksNaaSDisableAndValidateOnly(t *testing.T) {
	for _, value := range []*bool{nil, ptr.To(false)} {
		for _, validateOnly := range []bool{false, true} {
			w, reader := subTenantSafetyWorld(t)
			if _, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(true)}, "spec.naas")); err != nil {
				t.Fatal(err)
			}
			addSubTenantDependency(t, reader, "terminating-child", worldNS, w.row.ID, nil, true)
			before := w.cluster(t)
			req := updateRequest(w, &cloudv1.ClusterSpec{Naas: value}, "spec.naas")
			req.ValidateOnly = validateOnly
			if _, err := w.svc.UpdateCluster(t.Context(), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("value=%v validate_only=%t: got %v", value, validateOnly, err)
			}
			after := w.cluster(t)
			if after.ETag != before.ETag || after.Generation != before.Generation || string(after.Spec) != string(before.Spec) {
				t.Fatal("rejected NaaS disable mutated the parent")
			}
			if reader.calls != 1 {
				t.Fatalf("dependency reads = %d, want exactly one for disable", reader.calls)
			}
		}
	}
}

func TestSubTenantSafetyAllowsUnrelatedUpdatesAndNaaSDisableWithoutChildren(t *testing.T) {
	w, reader := subTenantSafetyWorld(t)
	addSubTenantDependency(t, reader, "child", worldNS, w.row.ID, nil, false)
	for _, req := range []*cloudv1.UpdateClusterRequest{
		updateRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(true)}, "spec.naas"),
		updateRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(false)}, pathDisplayName),
	} {
		if _, err := w.svc.UpdateCluster(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if reader.calls != 0 {
		t.Fatal("unrelated updates or enabling NaaS checked dependents")
	}
	if err := reader.direct.Delete(t.Context(), &cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: worldNS}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(false)}, "spec.naas")); err != nil {
		t.Fatal(err)
	}
}

func TestSubTenantSafetyPropagatesReadErrorsIncludingMissingKind(t *testing.T) {
	for _, readErr := range []error{
		errors.New("API reader unavailable"),
		context.Canceled,
		apierrors.NewNotFound(schema.GroupResource{Group: cloudv1alpha1.SchemeGroupVersion.Group, Resource: "subtenants"}, ""),
		apierrors.NewForbidden(schema.GroupResource{Group: cloudv1alpha1.SchemeGroupVersion.Group, Resource: "subtenants"}, "", errors.New("no list access")),
		&meta.NoKindMatchError{GroupKind: cloudv1alpha1.SchemeGroupVersion.WithKind("SubTenant").GroupKind()},
	} {
		w, reader := subTenantSafetyWorld(t)
		reader.err = readErr
		propagated := func(err error) bool {
			// The service's existing RPC boundary maps canceled contexts to
			// CANCELED; other Kubernetes/reader errors retain their cause.
			if errors.Is(readErr, context.Canceled) {
				return connect.CodeOf(err) == connect.CodeCanceled
			}
			return errors.Is(err, readErr)
		}
		before := w.cluster(t)
		_, err := w.svc.DeleteCluster(t.Context(), &cloudv1.DeleteClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID})
		if !propagated(err) {
			t.Fatalf("delete swallowed read failure: %v", err)
		}
		for _, validateOnly := range []bool{true, false} {
			req := updateRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(false)}, "spec.naas")
			req.ValidateOnly = validateOnly
			if _, err := w.svc.UpdateCluster(t.Context(), req); !propagated(err) {
				t.Fatalf("validate_only=%t swallowed read failure: %v", validateOnly, err)
			}
		}
		if w.cluster(t).ETag != before.ETag {
			t.Fatal("read failures mutated the parent")
		}
	}
}
