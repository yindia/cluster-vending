package cluster

import (
	"testing"

	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

const iamGroup = "iam.aws.m.upbound.io/v1beta1"

// iamKinds are the kinds of a component's IAM role (IRSA).
var iamKinds = []string{"Role", "Policy", "RolePolicyAttachment"}

// componentObjects are the management cluster's objects of an IAM kind
// labelled for component, not being deleted.
func (w world) componentObjects(t *testing.T, kind, component string) []unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(iamGroup)
	list.SetKind(kind + "List")
	if err := w.kube.List(t.Context(), list, client.MatchingLabels{crossplane.LabelComponent: component}); err != nil {
		t.Fatal(err)
	}
	var out []unstructured.Unstructured
	for _, u := range list.Items {
		if u.GetDeletionTimestamp().IsZero() {
			out = append(out, u)
		}
	}
	return out
}

// iamReady marks the component's IAM objects Ready, as the provider does
// once AWS has them.
func (w world) iamReady(t *testing.T, component string) {
	t.Helper()
	for _, kind := range iamKinds {
		for _, u := range w.componentObjects(t, kind, component) {
			u.Object["status"] = map[string]any{"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "reason": "Available"},
				map[string]any{"type": "Synced", "status": "True", "reason": "ReconcileSuccess"},
			}}
			if err := w.kube.Update(t.Context(), &u); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// irsaCatalogWorld is catalogClusterWorld with IRSA on the cluster and an
// IRSA role on mesh, installed and fingerprinted.
func irsaCatalogWorld(t *testing.T) (world, *catalogPlatform, *model.Cluster) {
	t.Helper()
	w, src := platformWorld(t)
	src.defs[0].Spec.CloudResources = &cloudv1alpha1.ComponentCloudResources{IAMRole: &cloudv1alpha1.ComponentIAMRole{
		Access:          cloudv1alpha1.ComponentIAMAccessIRSA,
		ServiceAccounts: []cloudv1alpha1.ComponentServiceAccount{{Namespace: "mesh-system", Name: "mesh"}},
		Policy:          `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["autoscaling:DescribeAutoScalingGroups"],"Resource":["*"]}]}`}}
	cp := &catalogPlatform{fakePlatform: src, admitted: true}
	w.svc.deps.Platform = cp
	w.helmConfig(t)
	// The cluster's IAM OIDC provider, with its ARN: the IRSA role trusts it.
	oidc := &awsiam.OpenIDConnectProvider{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS,
		Name: crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name) + "-oidc"}}
	oidc.Status.AtProvider.Arn = ptr.To("arn:aws:iam::444455556666:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE")
	if err := w.kube.Create(t.Context(), oidc); err != nil {
		t.Fatal(err)
	}
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.ReleaseChannel = cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE
	spec.GetAws().Irsa = ptr.To(true)
	if row.Spec, err = Encode(spec); err != nil {
		t.Fatal(err)
	}
	row = w.pass(t, row)
	w.deployed(t, "base")
	row = w.pass(t, row)
	for i := 0; i < 3 && w.release(t, "mesh") == nil; i++ { // role and policy, then their attachment
		w.iamReady(t, "mesh")
		row = w.pass(t, row)
	}
	w.deployed(t, "mesh")
	row = w.pass(t, row)
	if got := phases(storedPlatform(row)); got != "base=READY,mesh=READY" {
		t.Fatalf("installed: %s", got)
	}
	if applied := catalogOf(row).GetApplied(); applied["base"] == "" || applied["mesh"] == "" {
		t.Fatalf("not fingerprinted: %v", applied)
	}
	return w, cp, row
}

// A shipped definition a new build no longer has is deleted by
// catalog.Sync (the in-use finalizer keeps the object while a cluster still
// has its objects): a definition being deleted is off, so the cluster's
// catalog change removes the component through the gated removal, its
// Helm release and its IAM role, policy and attachment (IRSA) alike. This
// is how cluster-autoscaler leaves the clusters that have it.
func TestCatalogDeletedDefinitionIsUninstalledWithItsIAM(t *testing.T) {
	w, cp, row := irsaCatalogWorld(t)
	for _, kind := range iamKinds {
		if len(w.componentObjects(t, kind, "mesh")) == 0 {
			t.Fatalf("mesh was installed without its IAM %s", kind)
		}
	}
	now := metav1.Now()
	cp.defs[0].DeletionTimestamp = &now // mesh, held by its finalizer

	for i := 0; i < 6 && catalogOf(row).GetPhase() != catalogDone; i++ {
		row = w.pass(t, row)
	}

	c := catalogOf(row)
	if c.GetPhase() != catalogDone || c.GetApplied()["mesh"] != "" || c.GetApplied()["base"] == "" {
		t.Fatalf("catalog change = %v", c)
	}
	if w.release(t, "mesh") != nil || w.release(t, "base") == nil {
		t.Fatalf("mesh release %v, base release %v: want mesh removed, base kept", w.release(t, "mesh") != nil, w.release(t, "base") != nil)
	}
	for _, kind := range iamKinds {
		if left := w.componentObjects(t, kind, "mesh"); len(left) != 0 {
			t.Errorf("mesh's IAM %s is left: %d", kind, len(left))
		}
	}
	if got := phases(storedPlatform(row)); got != "base=READY" {
		t.Errorf("components after the removal: %s", got)
	}
}
