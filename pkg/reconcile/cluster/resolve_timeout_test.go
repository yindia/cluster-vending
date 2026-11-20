package cluster

import (
	"context"
	"testing"
	"time"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// hungEKS blocks every lookup until its context is done, as an AWS call
// that never answers does.
type hungEKS struct{}

func (hungEKS) EKSClusterExists(ctx context.Context, _ awscloud.Access, _, _ string) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func (hungEKS) ResolveEKSVersions(ctx context.Context, _ awscloud.Access, _ string, _ awscloud.VersionRequest) (awscloud.VersionResult, error) {
	<-ctx.Done()
	return awscloud.VersionResult{}, ctx.Err()
}

// A lookup that hangs is bounded: the reconcile gets a retryable
// Unavailable fault within the bound (not a permanent failure, and the
// worker is free again).
func TestHungEKSLookupsTimeOutAsUnavailable(t *testing.T) {
	const bound = 50 * time.Millisecond
	old := lookupTimeout
	lookupTimeout = bound
	t.Cleanup(func() { lookupTimeout = old })

	account := &model.CloudAccount{Base: model.Base{ID: "acct-1"}, AccountID: "111122223333",
		Identity: "arn:aws:iam::111122223333:role/provisioner"}
	row := &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1"}, Generation: 1}, Name: "prod", Region: "us-east-1"}
	s := New(reconcile.Deps{AWSClusters: hungEKS{}})

	for name, lookup := range map[string]func(ctx context.Context) error{
		"the EKS name check": func(ctx context.Context) error {
			// Nothing is recorded for the cluster: the store is not asked first.
			return s.lookupEKSName(ctx, row, account)
		},
		"the version lookup": func(ctx context.Context) error {
			return s.resolveVersions(ctx, account, &cloudv1.ClusterSpec{Region: "us-east-1"}, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			err := lookup(t.Context()) // the caller's context has no deadline

			if !fault.Is(err, fault.Unavailable) {
				t.Fatalf("err = %v, want an Unavailable fault", err)
			}
			if elapsed := time.Since(start); elapsed > 40*bound {
				t.Errorf("returned after %s, want about %s", elapsed, bound)
			}
			if c := FailureCondition(err); c.GetReason() != ReasonRetrying {
				t.Errorf("condition reason = %s, want %s", c.GetReason(), ReasonRetrying)
			}
		})
	}
}
