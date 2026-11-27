package cluster

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// A takeover before the plan is ready says the phase in words, what to
// wait for, and why the lookup fails.
func TestTakeOverBeforeThePlanSaysWhatToWaitFor(t *testing.T) {
	st, err := reconcilecluster.Encode(&cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_FAILED,
		Message: "AccessDenied: eks:DescribeCluster", Import: &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING}})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Cluster{State: reconcilecluster.StateImported, Status: st}
	row.ID = "c1"
	_, err = takeOverAllowed(row, &cloudv1.TakeOverClusterRequest{})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"status.import.phase is PLANNED", "cluster get c1", "the lookup is failing: AccessDenied: eks:DescribeCluster"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "IMPORT_PHASE_") {
		t.Errorf("%q names the enum", err)
	}
}
