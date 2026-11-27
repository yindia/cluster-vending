package network

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
)

func TestTakeOverBeforeThePlanSaysWhatToWaitFor(t *testing.T) {
	st, err := netreconcile.Encode(&cloudv1.NetworkStatus{Import: &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING}})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Network{State: netreconcile.StateImported, Status: st}
	row.ID = "n1"
	_, err = takeOverAllowed(row, &cloudv1.TakeOverNetworkRequest{})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "network get n1") ||
		strings.Contains(err.Error(), "lookup is failing") {
		t.Fatalf("err = %v", err)
	}
}
