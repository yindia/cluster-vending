package network

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
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
// build's render. It is admitted in the network's tier,
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
	if _, ok := s.deps.Platform.(reconcile.CodeAdmitter); ok {
		f.Epoch = reconcile.CodeEpoch()
		f.WaitForConvergence = f.Prev.GetPhase() == cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING
	}
	if f.Pending() {
		if a, ok := s.deps.Platform.(reconcile.CodeAdmitter); ok {
			// Publish a new target as Pending before asking a channel for its
			// target-bound admission. An old plan must not authorize new code.
			if f.Prev.GetPhase() != cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING && f.Prev.GetTarget() != f.Target() {
				f.Admitted, f.NotAdmitted = false, "waiting for the network's current render target to be planned"
			} else {
				admitted, why, err := a.NetworkCodeAdmitted(ctx, row.TenantID, row.ID)
				if err != nil {
					why = fmt.Sprintf("cannot read the code rollout of its clusters' channels: %v", err)
				}
				f.Admitted, f.NotAdmitted = admitted && err == nil, why
			}
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
	if complete {
		var err error
		complete, err = s.codeResourcesConverged(ctx, row, rendered)
		if err != nil {
			// Observation failures are not failed SSA attempts. Keep the apply
			// in flight and explain unsupported/lagging provider observations.
			st.Message = err.Error()
			reconcile.SetCode(ctx, st)
			return
		}
	}
	out, note := reconcile.CodeApplied(st, complete, failure, time.Now())
	reconcile.SetCode(ctx, out)
	if note != nil {
		reconcile.AddNote(ctx, *note)
	}
}

// A successful SSA is not cloud convergence. Read current provider generations
// directly; cached Ready=True from before the render must not finish a canary.
func (s *Reconciler) codeResourcesConverged(ctx context.Context, row *model.Network, rendered map[string]string) (bool, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return false, err
	}
	seen := 0
	for _, record := range records {
		key := record.APIVersion + "/" + record.Kind + "/" + record.Namespace + "/" + record.Name
		if _, expected := rendered[key]; !expected {
			continue
		}
		seen++
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(schema.FromAPIVersionAndKind(record.APIVersion, record.Kind))
		if err := s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: record.Namespace, Name: record.Name}, object); err != nil {
			return false, err
		}
		view := managed.View(managed.Ref{APIVersion: record.APIVersion, Kind: record.Kind, Namespace: record.Namespace, Name: record.Name}, object)
		_, managedResource, _ := unstructured.NestedFieldNoCopy(object.Object, "spec", "forProvider")
		if view.State == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED {
			return false, fmt.Errorf("network code resource %s failed: %s", record.Name, view.Message)
		}
		if managedResource && view.Generation > 0 && view.ObservedGeneration == 0 {
			return false, fmt.Errorf("network code resource %s has no Synced.observedGeneration; waiting for the supported provider's generation-aware observation", record.Name)
		}
		conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
		for _, raw := range conditions {
			condition, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// Upjet's async Observe may report ResourceUpToDate while an
			// operation is still running. Current Synced alone is insufficient.
			if (condition["type"] == "AsyncOperation" || condition["type"] == "LastAsyncOperation") && condition["status"] != "True" {
				return false, nil
			}
			if condition["type"] == "Ready" {
				observed, _ := condition["observedGeneration"].(int64)
				if observed > 0 && observed < view.Generation {
					return false, nil
				}
			}
		}
		if view.State != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY || view.Paused || managedResource && (view.Generation > view.ObservedGeneration || view.Synced != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE) {
			return false, nil
		}
	}
	return seen == len(rendered), nil
}
