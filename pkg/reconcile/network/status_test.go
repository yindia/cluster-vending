package network

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestNetworkStateFromResources(t *testing.T) {
	mr := func(state cloudv1.ManagedResourceState, msg string) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: "Route", Name: "r", State: state, Message: msg}
	}
	ready := mr(cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, "")
	waiting := mr(cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING, "cannot resolve references")
	failed := mr(cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, "UnauthorizedOperation")
	missing := mr(cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING, "not found")
	live := &model.Network{Resource: model.Resource{State: StateProvisioning}}
	for name, tc := range map[string]struct {
		row   *model.Network
		views []*cloudv1.ManagedResource
		want  cloudv1.NetworkState
	}{
		"all ready":             {live, []*cloudv1.ManagedResource{ready, ready}, cloudv1.NetworkState_NETWORK_STATE_READY},
		"waiting is not failed": {live, []*cloudv1.ManagedResource{ready, waiting}, cloudv1.NetworkState_NETWORK_STATE_PROVISIONING},
		"one failed":            {live, []*cloudv1.ManagedResource{ready, waiting, failed}, cloudv1.NetworkState_NETWORK_STATE_FAILED},
		"one missing":           {live, []*cloudv1.ManagedResource{ready, missing}, cloudv1.NetworkState_NETWORK_STATE_FAILED},
		"nothing yet":           {live, nil, cloudv1.NetworkState_NETWORK_STATE_PROVISIONING},
		"deleting":              {&model.Network{Resource: model.Resource{State: StateDeleting}}, []*cloudv1.ManagedResource{failed}, cloudv1.NetworkState_NETWORK_STATE_DELETING},
	} {
		if got := status(tc.row, tc.views, nil, nil); got.GetState() != tc.want {
			t.Errorf("%s: state = %s, want %s", name, got.GetState(), tc.want)
		}
	}
	st := status(live, []*cloudv1.ManagedResource{ready, waiting, failed}, nil, nil)
	if st.GetMessage() != "Route r: UnauthorizedOperation" || len(st.GetConditions()) != 2 || len(st.GetManagedResources()) != 3 {
		t.Errorf("failed status = %v", st)
	}
	if st := status(live, []*cloudv1.ManagedResource{ready, waiting}, nil, nil); st.GetMessage() != "1 of 2 resources ready, 1 waiting on others" {
		t.Errorf("progress = %q", st.GetMessage())
	}
}

// Whether a network is imported decides if it may ever be rendered from
// its spec: a status with a field this build does not know still says so.
func TestStoredImportSurvivesUnknownFields(t *testing.T) {
	row := &model.Network{Resource: model.Resource{
		Status: model.JSON(`{"import":{"phase":"IMPORT_PHASE_CONVERGED"},"aFieldFromAnotherVersion":{"x":1}}`)}}
	if imp := StoredImport(row); imp.GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_CONVERGED {
		t.Fatalf("storedImport = %v, want the import plan", imp)
	}
	if StoredImport(&model.Network{}) != nil {
		t.Error("a created network has an import plan")
	}
}

// Ready resources are not READY while a provider has not looked at the
// latest change.
func TestReadyNeedsTheProvidersToHaveCaughtUp(t *testing.T) {
	view := func(generation, observed int64) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: "Group", Name: "g", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY,
			Synced: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Generation: generation, ObservedGeneration: observed}
	}
	row := &model.Network{Resource: model.Resource{State: StateUpdating, Generation: 2, AppliedGeneration: 2}}
	for _, tt := range []struct {
		name string
		row  *model.Network
		view *cloudv1.ManagedResource
		want cloudv1.NetworkState
	}{
		{"caught up", row, view(3, 3), cloudv1.NetworkState_NETWORK_STATE_READY},
		{"change not seen yet", row, view(3, 2), cloudv1.NetworkState_NETWORK_STATE_UPDATING},
		{"generation not applied yet", &model.Network{Resource: model.Resource{State: StateUpdating, Generation: 2, AppliedGeneration: 1}},
			view(3, 3), cloudv1.NetworkState_NETWORK_STATE_UPDATING},
	} {
		if got := status(tt.row, []*cloudv1.ManagedResource{tt.view}, nil, nil).GetState(); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
}

// A VPC whose delete AWS refuses for dependencies says so, with where to
// look: what a cluster's controllers made (load balancers, their network
// interfaces and security groups) blocks it, and nothing the platform
// deletes will.
func TestDeletingNetworkNamesItsDependencies(t *testing.T) {
	row := &model.Network{Resource: model.Resource{State: StateDeleting}}
	vpc := &cloudv1.ManagedResource{Kind: "VPC", Name: "prod-vpc", ExternalName: "vpc-0abc", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING,
		Synced:  cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
		Message: "delete failed: deleting EC2 VPC (vpc-0abc): operation error EC2: DeleteVpc, api error DependencyViolation: The vpc 'vpc-0abc' has dependencies and cannot be deleted."}

	st := status(row, []*cloudv1.ManagedResource{vpc}, nil, nil)

	for _, want := range []string{"the VPC still has dependencies AWS reports", "DependencyViolation", "load balancers", "vpc-0abc"} {
		if !strings.Contains(st.GetMessage(), want) {
			t.Errorf("message = %q, want it to contain %q", st.GetMessage(), want)
		}
	}
	plain := status(row, []*cloudv1.ManagedResource{{Kind: "VPC", Name: "prod-vpc", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING}}, nil, nil)
	if strings.Contains(plain.GetMessage(), "dependencies") {
		t.Errorf("without the error: %q", plain.GetMessage())
	}
}
