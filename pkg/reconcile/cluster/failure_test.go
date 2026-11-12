package cluster

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// What went wrong applying a cluster is its status: FAILED with the
// message when it cannot be built as it is, the message alone when the
// attempt is simply tried again; never the text of an internal error.
func TestFailureBecomesStatus(t *testing.T) {
	running := cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING
	for _, tt := range []struct {
		name    string
		err     error
		state   cloudv1.ClusterState
		message string
		reason  string
	}{
		{"cannot be built", fmt.Errorf("apply: %w", fault.New(fault.FailedPrecondition, "network n is NETWORK_STATE_DELETING")),
			cloudv1.ClusterState_CLUSTER_STATE_FAILED, "network n is NETWORK_STATE_DELETING", ReasonFailed},
		{"tried again", fault.New(fault.Unavailable, "cannot read the cluster's EKS cluster"),
			running, "retrying: cannot read the cluster's EKS cluster", ReasonRetrying},
		{"internal", errors.New("etcdserver: leader changed at 10.0.0.1"), running, "retrying: " + internalFailure, ReasonRetrying},
	} {
		st := &cloudv1.ClusterStatus{State: running, Message: "0 of 3 resources ready"}
		withFailure(st, FailureCondition(tt.err))
		last := st.GetConditions()[len(st.GetConditions())-1]
		if st.GetState() != tt.state || st.GetMessage() != tt.message || last.GetType() != ConditionReconciled || last.GetReason() != tt.reason {
			t.Errorf("%s: %s %q, condition %v; want %s %q (%s)", tt.name, st.GetState(), st.GetMessage(), last, tt.state, tt.message, tt.reason)
		}
	}
	// A cluster being deleted stays DELETING, and no failure leaves the
	// status alone.
	deleting := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_DELETING}
	withFailure(deleting, FailureCondition(fault.New(fault.FailedPrecondition, "x")))
	if deleting.GetState() != cloudv1.ClusterState_CLUSTER_STATE_DELETING {
		t.Errorf("a failing deletion reads %s", deleting.GetState())
	}
	if FailureCondition(nil) != nil {
		t.Error("no error made a failure condition")
	}
}

// A call that changes the cluster stores it without the failure of the
// generation before; the rest of its status stays.
func TestClearFailure(t *testing.T) {
	st := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_FAILED,
		Import:     &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING},
		Conditions: []*cloudv1.Condition{{Type: "Ready"}, FailureCondition(fault.New(fault.Invalid, "x"))}}
	raw, err := Encode(st)
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Cluster{Resource: model.Resource{Status: raw}}
	if storedFailure(row) == nil {
		t.Fatal("the stored failure is not found")
	}
	ClearFailure(row)
	if storedFailure(row) != nil || StoredImport(row).GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING {
		t.Errorf("after clearing: %s", row.Status)
	}
	untouched := &model.Cluster{Resource: model.Resource{Status: model.JSON(`{"state":"CLUSTER_STATE_RUNNING"}`)}}
	ClearFailure(untouched)
	if string(untouched.Status) != `{"state":"CLUSTER_STATE_RUNNING"}` {
		t.Errorf("a status without a failure was rewritten: %s", untouched.Status)
	}
}

// The objects an import plans come back from the row exactly as they are
// applied: same kinds, names and apply bodies.
func TestImportObjectsSurviveTheRow(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	eks := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "tf-1",
		Annotations: map[string]string{crossplaneaws.ExternalNameAnnotation: "tf"}}}
	eks.Spec.ForProvider.Region = ptr.To("us-east-1")
	eks.Spec.ManagementPolicies = crossplaneaws.PolicyObserveLateInit
	pod := &awseks.PodIdentityAssociation{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "tf-1-identity-a"}}
	plan := corecluster.ImportPlan{Objects: []client.Object{eks, pod}, PodIdentities: map[string]string{"tf-1-identity-a": "a-0123"},
		Adoption: crossplaneaws.Adoption{ClusterRoleName: "tf-cluster"}}
	adoption, err := WithImportObjects(model.JSON(`{"ClusterRoleName":"tf-cluster"}`), plan, scheme)
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Cluster{Adoption: adoption}
	objects, identities, ok, err := storedImportObjects(row, scheme)
	if err != nil || !ok || len(objects) != 2 || identities["tf-1-identity-a"] != "a-0123" {
		t.Fatalf("stored import = %d objects, %v, %t (%v)", len(objects), identities, ok, err)
	}
	for i, obj := range plan.Objects {
		want, err1 := kube.ApplyObject(obj, scheme)
		got, err2 := kube.ApplyObject(objects[i], scheme)
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		a, _ := want.MarshalJSON()
		b, _ := got.MarshalJSON()
		if !bytes.Equal(a, b) || fmt.Sprintf("%T", obj) != fmt.Sprintf("%T", objects[i]) {
			t.Errorf("object %d changed in the row:\n%s\n%s", i, a, b)
		}
	}
	// What the cluster keeps of its roles is still read from the same column.
	if got := Adoption(row); got.ClusterRoleName != "tf-cluster" {
		t.Errorf("adoption = %+v", got)
	}
	// A cluster the platform created, or one imported before objects were
	// kept, has none.
	for _, raw := range []model.JSON{nil, model.JSON(`{"ClusterRoleName":"x"}`)} {
		if _, _, ok, err := storedImportObjects(&model.Cluster{Adoption: raw}, scheme); ok || err != nil {
			t.Errorf("adoption %s: stored objects %t (%v), want none", raw, ok, err)
		}
	}
}
