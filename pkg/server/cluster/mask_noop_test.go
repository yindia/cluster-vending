package cluster

import (
	"testing"

	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// A mask that names kubernetes_version or the add-ons with the value they
// already have changes nothing: no new generation, nothing to resolve again.
func TestMaskNamingAnUnchangedVersionOrAddonsStoresNothing(t *testing.T) {
	for _, path := range []string{pathKubernetesVersion, pathAddons} {
		t.Run(path, func(t *testing.T) {
			w := appliedWorld(t)
			ctx := t.Context()
			before, err := w.svc.deps.Store.Clusters().Get(ctx, worldTenant, w.row.ID)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := reconcilecluster.SpecOf(before)
			if err != nil {
				t.Fatal(err)
			}
			req := &cloudv1.UpdateClusterRequest{TenantId: worldTenant,
				Cluster:    &cloudv1.Cluster{Id: before.ID, Spec: spec},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{path}}}
			if _, err := w.svc.UpdateCluster(ctx, req); err != nil {
				t.Fatal(err)
			}
			after, err := w.svc.deps.Store.Clusters().Get(ctx, worldTenant, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Generation != before.Generation || after.ResolvedGeneration != before.ResolvedGeneration || after.State != before.State {
				t.Fatalf("generation %d -> %d, resolved %d -> %d, state %s -> %s",
					before.Generation, after.Generation, before.ResolvedGeneration, after.ResolvedGeneration, before.State, after.State)
			}
		})
	}
}
