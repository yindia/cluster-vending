package network

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// CodeChanging reports whether the network has a code change waiting for
// its tier of the rollout or being applied.
func CodeChanging(row *model.Network) bool {
	p := storedStatus(row).GetCodeChange().GetPhase()
	return p == cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING || p == cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING
}

// fingerprints are the set's objects as it applies them
// (sequence.Fingerprints).
func (s *Reconciler) fingerprints(set sequence.Set) (map[string]string, error) {
	out, err := sequence.Fingerprints(set, s.deps.Kube.Scheme())
	if err != nil {
		return nil, fault.New(fault.Internal, "fingerprint the network's objects: "+err.Error())
	}
	return out, nil
}

// codeChange decides the pass's code change of a network rendered from
// its spec whose generation is applied (reconcile.DecideCode): it returns
// whether the network is applied now, and the fingerprints of this
// build's render. It is admitted in the tier of its most careful cluster,
// before its clusters of that tier, unless a freeze holds it
// (reconcile.CodeAdmitter); a network applied by a build that recorded no
// fingerprints takes this render as applied.
func (s *Reconciler) codeChange(ctx context.Context, row *model.Network) (bool, map[string]string, error) {
	spec, err := SpecOf(row)
	if err != nil {
		return false, nil, err
	}
	account, layout, err := s.Plan(ctx, row.TenantID, spec)
	if err != nil {
		return false, nil, err
	}
	set, err := crossplaneaws.NetworkSet(RenderPlan(row, spec, account, layout))
	if err != nil {
		return false, nil, err
	}
	rendered, err := s.fingerprints(set)
	if err != nil {
		return false, nil, err
	}
	f := reconcile.CodeFacts{Applied: row.RenderHashes, Now: rendered, Behind: s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID),
		Prev: storedStatus(row).GetCodeChange(), Admitted: true, At: time.Now()}
	if f.Pending() {
		if a, ok := s.deps.Platform.(reconcile.CodeAdmitter); ok {
			admitted, why, err := a.NetworkCodeAdmitted(ctx, row.TenantID, row.ID)
			if err != nil {
				why = fmt.Sprintf("cannot read the code rollout of its clusters' channels: %v", err)
			}
			f.Admitted, f.NotAdmitted = admitted && err == nil, why
		}
	}
	d := reconcile.DecideCode(f)
	reconcile.SetCode(ctx, d.Status)
	if d.Adopt {
		return false, rendered, s.adoptFingerprints(ctx, row, rendered)
	}
	if d.Started {
		reconcile.AddNote(ctx, reconcile.CodeStarted(d.Status))
		s.logger().InfoContext(ctx, "applying a code change", "network_id", row.ID, "change", d.Status.GetSummary())
	}
	return d.Apply, rendered, nil
}

// adoptFingerprints records rendered as what the network has applied.
func (s *Reconciler) adoptFingerprints(ctx context.Context, row *model.Network, rendered map[string]string) error {
	before := row.RenderHashes
	row.RenderHashes = rendered
	err := s.deps.Store.Networks().Record(ctx, row)
	if errors.Is(err, repository.ErrConflict) {
		row.RenderHashes = before // changed meanwhile: the next pass looks again
		return nil
	}
	if err != nil {
		row.RenderHashes = before
		return fault.New(fault.Internal, "could not record the network's fingerprints: "+err.Error())
	}
	return nil
}

// codeApplied records how the apply of a code change ended
// (reconcile.CodeApplied).
func (s *Reconciler) codeApplied(ctx context.Context, row *model.Network, rendered map[string]string, failure error) {
	st := reconcile.CodeOf(ctx, nil)
	if st == nil {
		return
	}
	complete := failure == nil && row.AppliedGeneration >= row.Generation && maps.Equal(row.RenderHashes, rendered) &&
		!s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID)
	out, note := reconcile.CodeApplied(st, complete, failure, time.Now())
	reconcile.SetCode(ctx, out)
	if note != nil {
		reconcile.AddNote(ctx, *note)
	}
}
