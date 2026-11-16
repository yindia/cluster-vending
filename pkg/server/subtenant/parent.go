package subtenant

import (
	"context"
	"encoding/json"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

// claimParent changes both the Kubernetes resource version and the cloudstore
// etag. Concurrent cluster updates/deletes that read the previous parent must
// retry and observe the newly created dependent. This is not a transaction
// across two Kubernetes objects: the post-create check and controller finalizer
// remain necessary to close deletion/disable races safely.
func (s *Service) claimParent(ctx context.Context, parent *cloud.Cluster) error {
	return kubeError(retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current, err := s.parent(ctx, parent.Labels[cloudstore.LabelTenant], parent.Labels[cloudstore.LabelID])
		if err != nil {
			return err
		}
		if !parentUnchanged(parent, current) {
			return connect.NewError(connect.CodeAborted, "parent cluster incarnation changed while claiming it")
		}
		before := current.DeepCopy()
		fields := map[string]json.RawMessage{}
		if raw := current.Annotations[cloudstore.AnnotationRow]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &fields); err != nil || fields == nil {
				return connect.NewError(connect.CodeFailedPrecondition, "parent cluster metadata cannot be safely claimed")
			}
		}
		etag, err := json.Marshal(uuid.NewString())
		if err != nil {
			return err
		}
		fields["etag"] = etag
		encoded, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[cloudstore.AnnotationRow] = string(encoded)
		if err := s.deps.Kube.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		*parent = *current
		return nil
	}))
}

func parentUnchanged(expected, current *cloud.Cluster) bool {
	return expected.UID == current.UID && expected.Name == current.Name && expected.Namespace == current.Namespace && expected.Labels[cloudstore.LabelID] == current.Labels[cloudstore.LabelID]
}
