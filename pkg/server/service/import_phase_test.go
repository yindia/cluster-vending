package service

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestImportNotPlanned(t *testing.T) {
	blocked := &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED, Resources: []*cloudv1.ImportedResource{
		{Kind: "NatGateway", ExternalName: "nat-1", Disposition: cloudv1.Disposition_DISPOSITION_MATCHES},
		{Kind: "RouteTable", ExternalName: "rtb-1", Detail: "routes through an instance", Disposition: cloudv1.Disposition_DISPOSITION_BLOCKING},
	}}
	for _, tt := range []struct {
		name   string
		imp    *cloudv1.ImportStatus
		lookup string
		want   []string
		absent string
	}{
		{"discovering", &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING}, "",
			[]string{"still looking up its resources", "wait until status.import.phase is PLANNED", "cluster get c1"}, "IMPORT_PHASE_"},
		{"discovering, lookup failing", &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING}, "AccessDenied:\n eks:DescribeCluster",
			[]string{"the lookup is failing: AccessDenied: eks:DescribeCluster"}, "IMPORT_PHASE_"},
		{"no import yet", nil, "", []string{"no import plan"}, "IMPORT_PHASE_"},
		{"blocked", blocked, "", []string{"blocked", "RouteTable rtb-1: routes through an instance", "BLOCKING"}, "nat-1"},
		{"converged", &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_CONVERGED}, "", []string{"already taken over", "CONVERGED"}, "IMPORT_PHASE_"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ImportNotPlanned("cluster", "c1", tt.imp, tt.lookup)
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("code = %v", connect.CodeOf(err))
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%q lacks %q", err, w)
				}
			}
			if strings.Contains(err.Error(), tt.absent) {
				t.Errorf("%q has %q", err, tt.absent)
			}
		})
	}
}
