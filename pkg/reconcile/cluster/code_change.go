package cluster

import (
	"context"
	"errors"
	"fmt"
	"maps"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/rollout"
	planner "github.com/yindia/cluster-vending/pkg/upgrade"
)

// CodeChanging reports whether the cluster has a code change waiting for
// its turn or being applied.
func CodeChanging(row *model.Cluster) bool { return changing(storedStatus(row).GetCodeChange()) }

func changing(c *cloudv1.CodeChange) bool {
	p := c.GetPhase()
	return p == cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING || p == cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING
}

// fingerprintNodeGroup is the node group name a fingerprint renders with:
// Apply picks the live name each pass, a code change is what else differs.
const fingerprintNodeGroup = "fingerprint"

// fingerprints are the cluster's objects as plan renders them
// (sequence.Fingerprints).
func (s *Reconciler) fingerprints(plan crossplaneaws.ClusterPlan) (map[string]string, error) {
	plan.NodeGroupName = fingerprintNodeGroup
	out, err := sequence.Fingerprints(crossplaneaws.ClusterSet(plan), s.deps.Kube.Scheme())
	if err != nil {
		return nil, fault.New(fault.Internal, "fingerprint the cluster's objects: "+err.Error())
	}
	return out, nil
}

// codeChange decides the pass's code change of a cluster whose generation
// is applied (reconcile.DecideCode): what this build renders against what
// was applied. It returns whether the cluster is applied now, and the
// fingerprints of this build's render. A cluster applied by a build that
// recorded none takes this render as applied.
func (s *Reconciler) codeChange(ctx context.Context, row *model.Cluster) (bool, map[string]string, error) {
	spec, err := SpecOf(row)
	if err != nil {
		return false, nil, err
	}
	t, err := s.Target(ctx, row.TenantID, spec)
	if err != nil {
		return false, nil, err
	}
	rendered, err := s.fingerprints(s.RenderPlan(spec, row, t))
	if err != nil {
		return false, nil, err
	}
	f := reconcile.CodeFacts{Applied: row.RenderHashes, Now: rendered, Behind: s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID),
		Prev: storedStatus(row).GetCodeChange(), Retry: row.Retry != nil, At: now()}
	if f.Pending() {
		f.Frozen, f.FrozenWhy = s.frozen(ctx, row, spec)
		f.Admitted, f.NotAdmitted = s.codeAdmitted(ctx, row, spec)
	}
	d := reconcile.DecideCode(f)
	reconcile.SetCode(ctx, d.Status)
	if d.Adopt {
		return false, rendered, s.adoptFingerprints(ctx, row, rendered)
	}
	if d.Started {
		reconcile.AddNote(ctx, reconcile.CodeStarted(d.Status))
		s.logger().InfoContext(ctx, "applying a code change", "cluster_id", row.ID, "change", d.Status.GetSummary())
	}
	return d.Apply, rendered, nil
}

// adoptFingerprints records rendered as what the cluster has applied.
func (s *Reconciler) adoptFingerprints(ctx context.Context, row *model.Cluster, rendered map[string]string) error {
	before := row.RenderHashes
	row.RenderHashes = rendered
	err := s.deps.Store.Clusters().Record(ctx, row)
	if errors.Is(err, repository.ErrConflict) {
		row.RenderHashes = before // changed meanwhile: the next pass looks again
		return nil
	}
	if err != nil {
		row.RenderHashes = before
		return fault.New(fault.Internal, "could not record the cluster's fingerprints: "+err.Error())
	}
	return nil
}

// codeApplied records how the apply of a code change ended: done once
// what is applied is what this build renders, tried again after a
// failure, halted past the recoveries (reconcile.CodeApplied).
func (s *Reconciler) codeApplied(ctx context.Context, row *model.Cluster, rendered map[string]string, failure error) {
	st := reconcile.CodeOf(ctx, nil)
	if st == nil {
		return
	}
	complete := failure == nil && row.AppliedGeneration >= row.Generation && maps.Equal(row.RenderHashes, rendered) &&
		!s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID)
	out, note := reconcile.CodeApplied(st, complete, failure, now())
	reconcile.SetCode(ctx, out)
	if note != nil {
		reconcile.AddNote(ctx, *note)
	}
}

// codeAdmitted asks the cluster's channel's code rollout whether it may
// apply its code change now; it waits while its network's code change is
// pending (the network goes first). A cluster on no channel, or a source
// that cannot say, is admitted.
func (s *Reconciler) codeAdmitted(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec) (bool, string) {
	if why := s.networkCodePending(ctx, row, spec); why != "" {
		return false, why
	}
	channel, ok := namedChannel(spec)
	a, can := s.deps.Platform.(reconcile.CodeAdmitter)
	if !ok || !can {
		return true, ""
	}
	admitted, why, err := a.ClusterCodeAdmitted(ctx, channel, row.TenantID, row.Name)
	if err != nil {
		return false, fmt.Sprintf("cannot read channel %s's code rollout: %v", channel, err)
	}
	return admitted, why
}

// networkCodePending says why the cluster waits for its network's code
// change: the network has one pending and rolls out in the cluster's tier
// (its most careful cluster's, rollout.NetworkTier), before its clusters
// of that tier. Empty otherwise: clusters of earlier tiers go first.
func (s *Reconciler) networkCodePending(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec) string {
	id := spec.GetNetwork().GetNetworkId()
	n, err := s.deps.Store.Networks().Get(ctx, row.TenantID, id)
	if err != nil {
		return ""
	}
	st := &cloudv1.NetworkStatus{}
	if len(n.Status) == 0 || Lenient.Unmarshal(n.Status, st) != nil {
		return ""
	}
	switch st.GetCodeChange().GetPhase() {
	case cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING, cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING:
	default:
		return ""
	}
	siblings, err := s.deps.Store.Clusters().ListByNetwork(ctx, id)
	if err != nil {
		return "cannot read the clusters of network " + id + "; waiting"
	}
	tier := planner.Tier2
	for i := range siblings {
		if other, err := SpecOf(&siblings[i]); err == nil {
			tier = min(tier, tierOf(&siblings[i], other))
		}
	}
	if tierOf(row, spec) != tier {
		return ""
	}
	return fmt.Sprintf("waiting for network %s's code change first (it goes before its clusters of tier %d)", id, tier)
}

// tierOf is the cluster's rollout tier (a cluster with no tier is 0).
func tierOf(row *model.Cluster, spec *cloudv1.ClusterSpec) planner.Tier {
	return rollout.Tier(holdView(row, spec).Spec.Tier)
}
