package cluster

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// liveReaderInterval is how often the node pass applies the live reader's
// objects in a workload cluster (PROP-02 7.1 and open question 4): the
// ClusterRole and its binding, the priority level and the flow schema.
// Applied once they are, they only need putting back if someone removed
// them.
const liveReaderInterval = time.Hour

// applyLiveReader applies crossplaneaws.LiveReaderObjects with the
// controller's kubeconfig c, at most every liveReaderInterval per cluster. A
// failure is logged and tried again on the next pass; it never fails the
// pass: the cluster runs without live reads meanwhile.
func (s *Reconciler) applyLiveReader(ctx context.Context, c client.Client, row *model.Cluster) {
	if !s.liveReader.claim(row.ID, now()) {
		return
	}
	objects, err := crossplaneaws.LiveReaderObjects()
	if err != nil {
		s.logger().ErrorContext(ctx, "live reader: render the workload objects", "cluster_id", row.ID, "error", err)
		s.liveReader.forget(row.ID)
		return
	}
	for _, obj := range objects {
		err := call(ctx, func(ctx context.Context) error {
			return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner(kube.FieldOwner), client.ForceOwnership)
		})
		if err != nil {
			s.logger().WarnContext(ctx, "live reader: apply", "cluster_id", row.ID, "object", objectRef(obj), "error", err)
			s.liveReader.forget(row.ID)
			return
		}
	}
}
