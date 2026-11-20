package cluster

import (
	"errors"
	"strings"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func bigPlan(size int) corecluster.ImportPlan {
	eks := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "tf-1",
		Annotations: map[string]string{"big": strings.Repeat("x", size)}}}
	return corecluster.ImportPlan{Objects: []client.Object{eks}}
}

// A plan whose objects would not fit the stored adoption is refused with
// ErrImportTooLarge; the planned row is a blocked import saying why, not a
// Record that fails on every pass.
func TestImportPlanTooLargeIsBlocked(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WithImportObjects(nil, bigPlan(maxImportObjectsBytes+1), scheme); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("WithImportObjects = %v, want ErrImportTooLarge", err)
	}
	if _, err := WithImportObjects(nil, bigPlan(1024), scheme); err != nil {
		t.Fatalf("a small plan = %v, want stored", err)
	}

	s := New(reconcile.Deps{Kube: fake.NewClientBuilder().WithScheme(scheme).Build()})
	row := &model.Cluster{Resource: model.Resource{Generation: 1}, Region: "us-east-1", NetworkID: "net"}
	if err := s.planned(row, bigPlan(maxImportObjectsBytes+1)); err != nil {
		t.Fatalf("planned = %v, want a blocked plan, not an error", err)
	}
	imp := StoredImport(row)
	if row.State != StateBlocked || imp.GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED ||
		len(imp.GetResources()) != 1 || !strings.Contains(imp.GetResources()[0].GetDetail(), "too large") {
		t.Fatalf("state %s, import %v: want BLOCKED naming the size", row.State, imp)
	}
}
