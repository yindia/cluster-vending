package cluster

import (
	"context"
	"errors"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ClusterProto is the API view of a stored cluster with status st.
func ClusterProto(row *model.Cluster, spec *cloudv1.ClusterSpec, st *cloudv1.ClusterStatus) *cloudv1.Cluster {
	return &cloudv1.Cluster{
		TenantId: row.TenantID, Id: row.ID, Name: row.Name, DisplayName: row.DisplayName, Labels: row.Labels,
		Spec: spec, Status: st, Generation: row.Generation, Etag: row.ETag,
		CreateTime: timestamppb.New(row.CreatedAt), UpdateTime: timestamppb.New(row.UpdatedAt),
	}
}

// remember records a cluster's status as observed now, for GetCluster and
// ListClusters. It writes the status column only: the etag does not
// change. A cluster that is gone has nothing to record, and one that a
// call changed since row was read keeps what that call stored.
func (s *Reconciler) remember(ctx context.Context, row *model.Cluster, st *cloudv1.ClusterStatus) error {
	raw, err := Encode(st)
	if err != nil {
		return err
	}
	if string(raw) == string(row.Status) {
		return nil
	}
	err = s.deps.Store.Clusters().SetStatus(ctx, row.TenantID, row.ID, row.ETag, raw)
	if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrConflict) {
		return nil // gone, or changed since it was read: the next pass records it
	}
	if err == nil {
		row.Status = raw
	}
	return err
}

// LiveKubernetesVersion is the version the EKS cluster reports; empty
// before EKS created it.
func (s *Reconciler) LiveKubernetesVersion(ctx context.Context, row *model.Cluster) (string, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return "", storeError(err)
	}
	for _, r := range records {
		if r.Kind != "Cluster" {
			continue
		}
		c := &awseks.Cluster{}
		if err := s.getIgnoreMissing(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, c); err != nil {
			return "", fault.New(fault.Unavailable, "cannot read the EKS cluster: "+err.Error())
		}
		return deref(c.Status.AtProvider.Version), nil
	}
	return "", nil
}
