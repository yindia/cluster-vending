package cluster

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// withVersions gives the service an EKS lookup that resolves add-on
// versions ("v1-<kubernetes version>").
func withVersions(d *service.Deps) {
	d.AWSClusters = fakeLookup{}
	d.AWS = config.AWS{StaticKeys: &config.StaticKeys{AccessKeyID: "AKIA", SecretAccessKey: "x"}}
}

// appliedWorld is a world whose cluster is fully applied.
func appliedWorld(t *testing.T) world {
	t.Helper()
	w := newWorld(t, interceptor.Funcs{}, nil, withVersions)
	if err := w.svc.Reconcile(t.Context(), worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	return w
}

func planRequest(w world, spec *cloudv1.ClusterSpec, paths ...string) *cloudv1.UpdateClusterRequest {
	return &cloudv1.UpdateClusterRequest{TenantId: worldTenant, ValidateOnly: true,
		Cluster:    &cloudv1.Cluster{Id: w.row.ID, Spec: spec},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}
}

func changesOf(p *cloudv1.Plan, kind string, action cloudv1.PlannedAction) []*cloudv1.PlannedChange {
	var out []*cloudv1.PlannedChange
	for _, c := range p.GetChanges() {
		if c.GetKind() == kind && c.GetAction() == action {
			out = append(out, c)
		}
	}
	return out
}

func TestPlanKubernetesUpgradeIsDisruptive(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	before := w.cluster(t)
	req := planRequest(w, &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.35"}},
		"spec.control_plane.kubernetes_version")
	res, err := update(t, w, req)
	if err != nil {
		t.Fatal(err)
	}
	plan := res.GetPlan()
	const disruptive = cloudv1.PlannedAction_PLANNED_ACTION_DISRUPTIVE
	eks := changesOf(plan, "Cluster", disruptive)
	if len(eks) != 1 || eks[0].GetSpecPath() != "spec.control_plane.kubernetes_version" || eks[0].GetUnifiedDiff() == "" {
		t.Errorf("cluster changes = %v, want the version as DISRUPTIVE with a diff", eks)
	}
	if addons := changesOf(plan, "Addon", disruptive); len(addons) != len(crossplaneaws.DefaultAddons) {
		t.Errorf("%d add-on version changes, want one per add-on (%d): %v", len(addons), len(crossplaneaws.DefaultAddons), plan.GetChanges())
	}
	if ng := changesOf(plan, "NodeGroup", disruptive); len(ng) != 1 {
		t.Errorf("node group changes = %v, want its version as DISRUPTIVE", ng)
	}
	if !plan.GetApplicable() || plan.GetPlanId() == "" || len(plan.GetWarnings()) == 0 {
		t.Errorf("applicable %v, id %q, warnings %v", plan.GetApplicable(), plan.GetPlanId(), plan.GetWarnings())
	}
	// A plan changes nothing.
	after := w.cluster(t)
	live := &awseks.Cluster{}
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, live); err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation || after.ETag != before.ETag || live.Spec.ForProvider.Version != nil {
		t.Errorf("the plan changed the cluster: generation %d -> %d, live version %v", before.Generation, after.Generation, live.Spec.ForProvider.Version)
	}
}

func TestPlanInstanceTypeChangeReplacesTheNodeGroup(t *testing.T) {
	w := appliedWorld(t)
	spec := &cloudv1.ClusterSpec{DefaultNodePool: &cloudv1.NodePoolSpec{InstanceTypes: []string{"m7i.xlarge"}}}
	res, err := update(t, w, planRequest(w, spec, "spec.default_node_pool.instance_types"))
	if err != nil {
		t.Fatal(err)
	}
	plan := res.GetPlan()
	created := changesOf(plan, "NodeGroup", cloudv1.PlannedAction_PLANNED_ACTION_CREATE)
	deleted := changesOf(plan, "NodeGroup", cloudv1.PlannedAction_PLANNED_ACTION_DELETE)
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	if len(created) != 1 || !created[0].GetRequiresReplacement() || created[0].GetName() == prefix+"-default" {
		t.Errorf("created = %v, want the replacement node group", created)
	}
	if len(deleted) != 1 || !deleted[0].GetRequiresReplacement() || deleted[0].GetName() != prefix+"-default" {
		t.Errorf("deleted = %v, want the current node group, requiring replacement", deleted)
	}
	if rejected := changesOf(plan, "NodeGroup", cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE); len(rejected) != 0 || !plan.GetApplicable() {
		t.Errorf("rejected %v, applicable %v: a replacement is not an immutable change", rejected, plan.GetApplicable())
	}
}

func TestUpdateWithAStalePlanIsRefused(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	spec := &cloudv1.ClusterSpec{DefaultNodePool: &cloudv1.NodePoolSpec{ScalingConfig: &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 9}}}
	req := planRequest(w, spec, "spec.default_node_pool.scaling_config.max_size")
	planned, err := update(t, w, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := changesOf(planned.GetPlan(), "NodeGroup", cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE); len(got) != 1 ||
		got[0].GetSpecPath() != "spec.default_node_pool.scaling_config.max_size" {
		t.Fatalf("plan = %v, want max size in place", planned.GetPlan().GetChanges())
	}

	// Something changes the node group after the plan.
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	ng := &awseks.NodeGroup{}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix + "-default"}, ng); err != nil {
		t.Fatal(err)
	}
	ng.Annotations = map[string]string{"touched": "true"}
	if err := w.kube.Update(ctx, ng); err != nil {
		t.Fatal(err)
	}
	req.ValidateOnly, req.PlanId = false, planned.GetPlan().GetPlanId()
	if _, err := update(t, w, req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale plan: %v, want FAILED_PRECONDITION", err)
	}

	// Planned again, the same request applies.
	req.ValidateOnly, req.PlanId = true, ""
	planned, err = update(t, w, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ValidateOnly, req.PlanId = false, planned.GetPlan().GetPlanId()
	res, err := update(t, w, req)
	if err != nil || res.GetPlan().GetPlanId() != req.GetPlanId() {
		t.Fatalf("apply with a fresh plan: %v, plan %q", err, res.GetPlan().GetPlanId())
	}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix + "-default"}, ng); err != nil ||
		*ng.Spec.ForProvider.ScalingConfig.MaxSize != 9 {
		t.Errorf("node group max size after the update: %v (%v), want 9", ng.Spec.ForProvider.ScalingConfig, err)
	}
}

// A Kubernetes upgrade is planned in its steps: the control plane at once,
// node groups and add-ons once it runs the new version.
func TestPlanShowsUpgradeStaging(t *testing.T) {
	w := appliedWorld(t)
	ctx := t.Context()
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	// EKS reports the version it runs.
	const running, target = "1.34", "1.35"
	eks := &awseks.Cluster{}
	if err := w.kube.Get(ctx, client.ObjectKey{Namespace: worldNS, Name: prefix}, eks); err != nil {
		t.Fatal(err)
	}
	eks.Status.AtProvider.Version = ptr.To(running)
	if err := w.kube.Update(ctx, eks); err != nil {
		t.Fatal(err)
	}
	// Its add-ons run the versions of that Kubernetes version.
	addons := &awseks.AddonList{}
	if err := w.kube.List(ctx, addons, client.InNamespace(worldNS)); err != nil {
		t.Fatal(err)
	}
	for i := range addons.Items {
		addons.Items[i].Spec.ForProvider.AddonVersion = ptr.To("v1-" + running)
		if err := w.kube.Update(ctx, &addons.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	res, err := update(t, w, planRequest(w, &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: target}},
		"spec.control_plane.kubernetes_version"))
	if err != nil {
		t.Fatal(err)
	}
	plan := res.GetPlan()
	const disruptive = cloudv1.PlannedAction_PLANNED_ACTION_DISRUPTIVE
	control := changesOf(plan, "Cluster", disruptive)
	if len(control) != 1 || control[0].GetWaitsFor() != "" {
		t.Errorf("control plane change = %v, want it made at once", control)
	}
	later := append(changesOf(plan, "NodeGroup", disruptive), changesOf(plan, "Addon", disruptive)...)
	if len(later) != 1+len(crossplaneaws.DefaultAddons) {
		t.Fatalf("%d node group and add-on changes, want %d: %v", len(later), 1+len(crossplaneaws.DefaultAddons), plan.GetChanges())
	}
	for _, c := range later {
		if c.GetWaitsFor() != "the control plane runs Kubernetes "+target {
			t.Errorf("%s %s waits for %q, want the control plane's new version", c.GetKind(), c.GetName(), c.GetWaitsFor())
		}
	}
}

// A change that needs another resource first is planned in full, with what
// it waits for and "(known after apply)" for the value it does not have
// yet; nothing that merely waits is listed as deleted.
func TestPlanShowsWhatWaitsForAnotherResource(t *testing.T) {
	w := appliedWorld(t)
	prefix := crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name)
	on := &cloudv1.ClusterSpec{ControlPlane: &cloudv1.ControlPlaneSpec{SecretsEncryption: &cloudv1.SecretsEncryption{Enabled: true}}}
	res, err := update(t, w, planRequest(w, on, pathSecretsEncryption))
	if err != nil {
		t.Fatal(err)
	}
	plan := res.GetPlan()
	key := changesOf(plan, "Key", cloudv1.PlannedAction_PLANNED_ACTION_CREATE)
	if len(key) != 1 || key[0].GetName() != prefix+"-secrets-key" || key[0].GetWaitsFor() != "" {
		t.Errorf("key = %v, want it created at once", key)
	}
	encrypt := changesOf(plan, "Cluster", cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE)
	if len(encrypt) == 0 {
		t.Fatalf("no change planned for the EKS cluster: %v", plan.GetChanges())
	}
	found := false
	for _, c := range encrypt {
		if c.GetSpecPath() != pathSecretsEncryption {
			continue
		}
		if c.GetWaitsFor() != "the encryption key exists" {
			t.Errorf("%s waits for %q, want the key", c.GetDetail(), c.GetWaitsFor())
		}
		found = found || strings.Contains(c.GetDetail(), crossplaneaws.KnownAfterApply)
	}
	if !found {
		t.Errorf("the key ARN is not planned as %q: %v", crossplaneaws.KnownAfterApply, encrypt)
	}
	if deleted := plan.GetChanges(); len(changesOf(plan, "Cluster", cloudv1.PlannedAction_PLANNED_ACTION_DELETE))+
		len(changesOf(plan, "NodeGroup", cloudv1.PlannedAction_PLANNED_ACTION_DELETE)) != 0 || !plan.GetApplicable() {
		t.Errorf("a waiting resource is planned as deleted, or the plan is not applicable: %v", deleted)
	}
}
