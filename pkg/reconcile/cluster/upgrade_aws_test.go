package cluster

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// awsWorld is a world whose cluster runs Kubernetes version, applied.
func awsWorld(t *testing.T, version string) world {
	t.Helper()
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.ControlPlane.KubernetesVersion = version
	if row.Spec, err = Encode(spec); err != nil {
		t.Fatal(err)
	}
	row.Generation++
	row.ResolvedGeneration = row.Generation
	if err := w.store.Clusters().Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	return w
}

// eksReports makes the EKS cluster object report version, as the provider
// does once it observed it.
func (w world) eksReports(t *testing.T, version string) *unstructured.Unstructured {
	t.Helper()
	u := w.eksObject(t)
	if err := unstructured.SetNestedField(u.Object, version, "status", "atProvider", "version"); err != nil {
		t.Fatal(err)
	}
	if err := w.kube.Update(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func (w world) eksObject(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "eks.aws.m.upbound.io", Version: "v1beta1", Kind: "Cluster"})
	key := client.ObjectKey{Namespace: worldNS, Name: crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)}
	if err := w.kube.Get(t.Context(), key, u); err != nil {
		t.Fatal(err)
	}
	return u
}

// AWS upgrades a control plane by force when its minor's support ends:
// EKS then reports a minor newer than the spec. The stored spec follows
// what EKS runs (rendering the older one is an update EKS refuses), with a
// ControlPlaneUpgradedByAWS warning, and the EKS object is applied at it.
func TestTheSpecFollowsAnAWSForceUpgrade(t *testing.T) {
	w := awsWorld(t, "1.35")
	before := w.cluster(t)
	w.eksReports(t, "1.36")

	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.GetControlPlane().GetKubernetesVersion(); got != "1.36" {
		t.Fatalf("spec version %q after EKS reported 1.36, want 1.36", got)
	}
	if row.Generation <= before.Generation {
		t.Fatalf("generation %d, want above %d: the spec changed", row.Generation, before.Generation)
	}
	found := false
	for _, e := range storedStatus(row).GetRecentEvents() {
		if e.GetReason() == reconcile.ReasonControlPlaneUpgradedByAWS && e.GetType() == cloudv1.EventType_EVENT_TYPE_WARNING {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s warning in %v", reconcile.ReasonControlPlaneUpgradedByAWS, storedStatus(row).GetRecentEvents())
	}
	if got, _, _ := unstructured.NestedString(w.eksObject(t).Object, "spec", "forProvider", "version"); got != "1.36" {
		t.Fatalf("the EKS object is applied at %q, want 1.36", got)
	}
}

// EKS behind the spec (an upgrade under way) or on its minor leaves the
// spec alone.
func TestTheSpecIgnoresAnEKSThatIsNotAhead(t *testing.T) {
	for _, reported := range []string{"1.34", "1.35", "1.35.2"} {
		t.Run(reported, func(t *testing.T) {
			w := awsWorld(t, "1.35")
			w.eksReports(t, reported)
			before := w.cluster(t)
			if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
				t.Fatal(err)
			}
			row := w.cluster(t)
			spec, err := SpecOf(row)
			if err != nil {
				t.Fatal(err)
			}
			if got := spec.GetControlPlane().GetKubernetesVersion(); got != "1.35" || row.Generation != before.Generation {
				t.Fatalf("spec version %q generation %d (was %d) with EKS on %s; want 1.35, unchanged", got, row.Generation, before.Generation, reported)
			}
			for _, e := range storedStatus(row).GetRecentEvents() {
				if e.GetReason() == reconcile.ReasonControlPlaneUpgradedByAWS {
					t.Fatalf("unexpected %s", e.GetReason())
				}
			}
		})
	}
}
