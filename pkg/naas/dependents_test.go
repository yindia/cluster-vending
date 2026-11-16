package naas

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func TestClusterDependentsIncludesRefsUIDLabelsAndTerminatingChildren(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	if !scheme.Recognizes(cloudv1alpha1.SchemeGroupVersion.WithKind("SubTenantList")) {
		t.Fatal("production scheme does not register SubTenantList")
	}
	parent := ClusterIdentity{Namespace: "tenant-acme", Name: "prod", ID: "parent-id", UID: types.UID("parent-uid")}
	children := []cloudv1alpha1.SubTenant{
		{ObjectMeta: metav1.ObjectMeta{Name: "unlabeled", Namespace: parent.Namespace}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: cloudv1alpha1.SubTenantClusterReference{ID: parent.ID}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "uid-only", Namespace: parent.Namespace}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: cloudv1alpha1.SubTenantClusterReference{UID: string(parent.UID)}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "id-label", Namespace: parent.Namespace, Labels: map[string]string{cloudstore.LabelClusterID: parent.ID}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "name-label", Namespace: parent.Namespace, Labels: map[string]string{cloudv1alpha1.LabelCluster: parent.Name}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "different-parent", Namespace: parent.Namespace}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: cloudv1alpha1.SubTenantClusterReference{ID: "different-id", Name: parent.Name, UID: "different-uid"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "other-namespace", Namespace: "tenant-other"}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: cloudv1alpha1.SubTenantClusterReference{ID: parent.ID, UID: string(parent.UID)}}},
	}
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	for i := range children {
		if err := k.Create(t.Context(), &children[i]); err != nil {
			t.Fatal(err)
		}
	}
	terminating := &cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: "terminating", Namespace: parent.Namespace, Finalizers: []string{"test/cleanup"}}, Spec: cloudv1alpha1.SubTenantSpec{ClusterRef: cloudv1alpha1.SubTenantClusterReference{ID: parent.ID}}}
	if err := k.Create(t.Context(), terminating); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(t.Context(), terminating); err != nil {
		t.Fatal(err)
	}
	names, err := ClusterDependents(t.Context(), k, parent)
	if err != nil || len(names) != 5 {
		t.Fatalf("dependents = %v, %v; want 5 including terminating/unlabeled/UID", names, err)
	}
	// Reconciler callers with row identity must resolve the current CR UID.
	obj := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: parent.Name, Namespace: parent.Namespace, UID: parent.UID}}
	if err := k.Create(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	parent.UID = ""
	if found, err := ClusterDependents(t.Context(), k, parent); err != nil || len(found) != 5 {
		t.Fatalf("resolved parent UID: %v %v", found, err)
	}
	if err := k.Delete(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if _, err := ClusterDependents(t.Context(), k, parent); err == nil {
		t.Fatal("missing parent identity was treated as no UID-based dependents")
	}
}

func TestClusterDependentsRejectsMissingReader(t *testing.T) {
	if _, err := ClusterDependents(t.Context(), nil, ClusterIdentity{}); err == nil {
		t.Fatal("missing reader was treated as no dependents")
	}
}

func TestParentBlockedMessageBoundsNamesAndKeepsTotal(t *testing.T) {
	names := make([]string, 23)
	for i := range names {
		names[i] = fmt.Sprintf("child-%02d", i)
	}
	message := ParentBlockedMessage("Cluster deletion", names)
	if !strings.Contains(message, "23 SubTenants") || !strings.Contains(message, "child-00") || !strings.Contains(message, "child-09") || !strings.Contains(message, "and 13 more") || strings.Contains(message, "child-10") {
		t.Fatalf("unbounded or incorrect message: %s", message)
	}
}
