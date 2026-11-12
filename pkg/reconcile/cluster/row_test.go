package cluster

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository/mock"
	"github.com/yindia/cluster-vending/pkg/fault"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// eksNames answers the EKS name lookup, and counts the questions.
type eksNames struct {
	exists bool
	err    error
	asked  *int
}

func (e eksNames) EKSClusterExists(context.Context, awscloud.Access, string, string) (bool, error) {
	*e.asked++
	return e.exists, e.err
}

func (e eksNames) ResolveEKSVersions(context.Context, awscloud.Access, string, awscloud.VersionRequest) (awscloud.VersionResult, error) {
	return awscloud.VersionResult{}, nil
}

// The platform's own clusters of a name are found in the store: no look
// at the cloud is needed to refuse a second one.
func TestNameTakenReadsTheStoreOnly(t *testing.T) {
	same := model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1"}}, Name: "prod", Region: "us-east-1", NetworkID: "n-1"}
	other := same
	other.Region = "eu-west-1"
	for name, tt := range map[string]struct {
		existing []model.Cluster
		taken    bool
	}{
		"free":                        {nil, false},
		"a cluster of the name":       {[]model.Cluster{same}, true},
		"a cluster in another region": {[]model.Cluster{other}, true}, // names are the tenant's
	} {
		ctrl := gomock.NewController(t)
		store, clusters := mock.NewMockStore(ctrl), mock.NewMockClusters(ctrl)
		store.EXPECT().Clusters().Return(clusters).AnyTimes()
		clusters.EXPECT().ListByName(gomock.Any(), "acme", "prod").Return(tt.existing, nil)
		asked := 0
		s := New(reconcile.Deps{Store: store, AWSClusters: eksNames{exists: true, asked: &asked}})
		err := s.NameTaken(t.Context(), "acme", "prod")
		if fault.Is(err, fault.AlreadyExists) != tt.taken || (err != nil) != tt.taken {
			t.Errorf("%s: %v, want taken %t", name, err, tt.taken)
		}
		if asked != 0 {
			t.Errorf("%s: the store check asked EKS %d times", name, asked)
		}
	}
}

// EKS is asked for the name only for a cluster nothing was created for
// yet: once the platform created its resources, the EKS cluster of the
// name is its own.
func TestEKSNameIsCheckedBeforeAnythingIsCreated(t *testing.T) {
	account := &model.CloudAccount{Base: model.Base{ID: "acct-1"}, AccountID: "111122223333",
		Identity: "arn:aws:iam::111122223333:role/provisioner"}
	fresh := func() *model.Cluster {
		return &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1"}, Generation: 1}, Name: "prod", Region: "us-east-1"}
	}
	resolved := fresh()
	resolved.Generation, resolved.ResolvedGeneration = 2, 1
	imported := fresh()
	imported.Imported = true
	for name, tt := range map[string]struct {
		row      *model.Cluster
		records  []model.ManagedResource
		lookup   eksNames
		asked    int
		wantKind fault.Kind
	}{
		"free":                       {fresh(), nil, eksNames{}, 1, 0},
		"taken":                      {fresh(), nil, eksNames{exists: true}, 1, fault.AlreadyExists},
		"credentials cannot ask":     {fresh(), nil, eksNames{err: awscloud.ErrAccessDenied}, 1, fault.FailedPrecondition},
		"the lookup did not answer":  {fresh(), nil, eksNames{err: context.DeadlineExceeded}, 1, fault.Unavailable},
		"resources exist already":    {fresh(), []model.ManagedResource{{Kind: "Role", Name: "prod-cluster-role"}}, eksNames{exists: true}, 0, 0},
		"resolved before, an update": {resolved, nil, eksNames{exists: true}, 0, 0},
		"an imported cluster":        {imported, nil, eksNames{exists: true}, 0, 0},
	} {
		ctrl := gomock.NewController(t)
		store, records := mock.NewMockStore(ctrl), mock.NewMockManagedResources(ctrl)
		store.EXPECT().ManagedResources().Return(records).AnyTimes()
		records.EXPECT().ListByOwner(gomock.Any(), model.OwnerCluster, "c-1").Return(tt.records, nil).AnyTimes()
		asked := 0
		tt.lookup.asked = &asked
		s := New(reconcile.Deps{Store: store, AWSClusters: tt.lookup})
		err := s.checkEKSName(t.Context(), tt.row, account)
		if (tt.wantKind == 0) != (err == nil) || (tt.wantKind != 0 && !fault.Is(err, tt.wantKind)) {
			t.Errorf("%s: %v, want fault kind %d", name, err, tt.wantKind)
		}
		if asked != tt.asked {
			t.Errorf("%s: EKS was asked %d times, want %d", name, asked, tt.asked)
		}
	}
}

// A taken EKS name fails the cluster for good (it is not retried as if
// the cloud had not answered): the tenant must choose another name.
func TestATakenNameIsAFailureNotARetry(t *testing.T) {
	c := FailureCondition(fault.New(fault.AlreadyExists, "account 111122223333 already has an EKS cluster named \"prod\""))
	if c.GetReason() != ReasonFailed {
		t.Errorf("reason = %s, want %s", c.GetReason(), ReasonFailed)
	}
	if c := FailureCondition(fault.New(fault.Unavailable, "the AWS lookup failed")); c.GetReason() != ReasonRetrying {
		t.Errorf("a lookup that did not answer = %s, want %s", c.GetReason(), ReasonRetrying)
	}
}
