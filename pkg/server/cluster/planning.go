package cluster

import (
	"context"

	"slices"

	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/kube"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/planning"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// planUpdate computes what applying row with spec (both already carrying
// the update, not stored) would do, changing nothing. The cluster is
// rendered twice and each object compared with the live one (a server-side
// apply dry run): once as it ends up, once as the update's first apply
// renders it. A change of the first that the second does not have happens
// in a later step, and says what it waits for (waits_for): node groups and
// add-ons follow the control plane during a Kubernetes upgrade, and some
// resources need another one to exist first (crossplaneaws.ClusterPlan
// Waits). Values only known then read "(known after apply)". The
// deletions Prune and the node group retirement would make are listed
// too; Usages (they only order deletions) are not. The plan's ID covers
// req and the live objects' versions.
func (s *ClusterService) planUpdate(ctx context.Context, req *cloudv1.UpdateClusterRequest, row *model.Cluster,
	spec *cloudv1.ClusterSpec, t reconcilecluster.Target,
) (*cloudv1.Plan, error) {

	metadataOnly := len(req.GetUpdateMask().GetPaths()) > 0
	for _, path := range req.GetUpdateMask().GetPaths() {
		if path != pathDisplayName && !slices.Contains(releasePaths, path) {
			metadataOnly = false
		}
	}
	if metadataOnly {
		id, err := planID(req, []string{"cluster=" + row.ID + "/" + row.ETag})
		return &cloudv1.Plan{Applicable: true, PlanId: id, Warnings: []string{"this update changes platform metadata; subsequent automatic changes follow their normal rollout gates"}}, err
	}
	final := s.RenderPlan(spec, row, t)
	name, retiring, err := s.NodeGroupName(ctx, row, final)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the cluster's node group")
	}
	final.NodeGroupName = name
	if err := s.FillLive(ctx, row, &final); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the cluster's resources")
	}
	// first is what the update's first apply renders (see apply).
	first := final
	first.Addons = slices.Clone(final.Addons)
	gated, err := s.GateUpgrade(ctx, row, &first)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the cluster's EKS cluster")
	}
	waits := first.Waits()
	final.AssumeLive()
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	replacing := len(retiring) > 0
	plan := &cloudv1.Plan{Applicable: true}
	versions, rendered, err := s.renderChanges(ctx, final, replacing, plan)
	if err != nil {
		return nil, err
	}
	now := &cloudv1.Plan{}
	if _, _, err := s.renderChanges(ctx, first, replacing, now); err != nil {
		return nil, err
	}
	immediate := map[string]bool{}
	for _, c := range now.GetChanges() {
		immediate[corecluster.ChangeKey(c)] = true
	}
	for _, c := range plan.GetChanges() {
		switch reason, waiting := waits[c.GetKind()]; {
		case immediate[corecluster.ChangeKey(c)]:
		case waiting:
			c.WaitsFor = reason
		case gated:
			c.WaitsFor = "the control plane runs Kubernetes " + final.KubernetesVersion
		}
	}

	entries := make([]planning.Entry, 0, len(records))
	for _, record := range records {
		entries = append(entries, planning.Entry{Ref: managed.Ref{APIVersion: record.APIVersion, Kind: record.Kind, Namespace: record.Namespace, Name: record.Name, ExternalName: record.ExternalName}, Retained: record.CarriedOver})
	}
	removalVersions, err := (planning.Snapshot{Client: s.deps.Kube, Reader: s.deps.Reader()}).Removals(ctx, entries, rendered, plan)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	versions = append(versions, removalVersions...)
	for _, change := range plan.Changes {
		if change.Action == cloudv1.PlannedAction_PLANNED_ACTION_DELETE && change.Kind == reconcilecluster.KindNodeGroup {
			change.RequiresReplacement = true
			change.Detail = "replaced by " + name + "; deleted once the replacement is ready"
			if change.GetRetainsCloudResource() {
				change.Detail = "replaced by " + name + "; the old cloud resource is retained"
			}
			change.WaitsFor = "node group " + name + " is ready"
		}
	}
	if change := s.encryptionChange(row, spec, final, plan.Changes); change != nil {
		plan.Changes = append(plan.Changes, change)
	}
	for _, c := range plan.Changes {
		switch c.GetAction() {
		case cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE:
			plan.Applicable = false
		case cloudv1.PlannedAction_PLANNED_ACTION_DISRUPTIVE:
			if c.GetKind() == "Cluster" && strings.HasSuffix(c.GetSpecPath(), "kubernetes_version") {
				plan.Warnings = append(plan.Warnings, "Kubernetes upgrade: the control plane is upgraded first (about 10 minutes); "+
					"node groups and add-ons follow once it reports the new version (waits_for), nodes are replaced one by one")
			}
		default:
		}
	}
	if warning := crossplaneaws.DedicatedPoolWarning(final.NodePool); warning != "" {
		plan.Warnings = append(plan.Warnings, warning)
	}
	capsuleVersions, err := s.planNaaS(ctx, row, spec, plan)
	if err != nil {
		return nil, err
	}
	versions = append(versions, capsuleVersions...)
	versions = append(versions, "cluster="+row.ID+"/"+row.ETag)
	plan.PlanId, err = planID(req, versions)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	return plan, nil
}

// renderChanges renders a cluster plan and adds to plan what applying it
// changes: CREATE for objects that do not exist, and the fields that
// differ on those that do. It returns the live objects' versions (for the
// plan ID) and which objects the render has.
func (s *ClusterService) renderChanges(ctx context.Context, render crossplaneaws.ClusterPlan, replacing bool, plan *cloudv1.Plan,
) ([]string, map[string]bool, error) {

	snapshot := planning.Snapshot{Client: s.deps.Kube, Reader: s.deps.Reader(), Changes: corecluster.FieldChanges,
		Prepare: func(body *unstructured.Unstructured) {
			if body.GetKind() == "NodeGroup" {
				crossplaneaws.DesiredSizeAtCreateOnly(body)
			}
		},
		CreateDetail: func(body *unstructured.Unstructured) string { return corecluster.CreateDetail(body, replacing) },
		Replacing:    func(body *unstructured.Unstructured) bool { return replacing && body.GetKind() == "NodeGroup" },
	}
	versions, rendered, err := snapshot.Render(ctx, crossplaneaws.ClusterResources(render), plan)
	return versions, rendered, service.ToConnect(err)
}

// encryptionChange is the refusal of an update that turns the secrets
// encryption off or moves it to another key, unless the field comparison
// already lists one: UpdateCluster refuses such an update whatever the
// objects' comparison shows.
func (s *ClusterService) encryptionChange(row *model.Cluster, spec *cloudv1.ClusterSpec, render crossplaneaws.ClusterPlan,
	changes []*cloudv1.PlannedChange,
) *cloudv1.PlannedChange {
	stored, err := reconcilecluster.SpecOf(row)
	if err != nil || corecluster.EncryptionKept(corecluster.Encryption(stored), corecluster.Encryption(spec)) {
		return nil
	}
	for _, c := range changes {
		if c.GetSpecPath() == pathSecretsEncryption && c.GetAction() == cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE {
			return nil
		}
	}
	return &cloudv1.PlannedChange{ApiVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "Cluster", Name: render.Prefix,
		ExternalName: render.ClusterName, SpecPath: pathSecretsEncryption,
		Action: cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE,
		Detail: "secrets encryption cannot be turned off or moved to another key once it is on: EKS cannot change it"}
}

// planID hashes the request (without validate_only and plan_id) and the
// live objects' versions: the same request against unchanged resources
// gives the same ID.
func planID(req *cloudv1.UpdateClusterRequest, versions []string) (string, error) {
	r := proto.CloneOf(req)
	r.ValidateOnly, r.PlanId = false, ""
	return planning.ID(r, versions)
}

// Capsule Releases are component-owned, outside the cluster-owned inventory
// rendered above. Describe the NaaS transition explicitly and bind review to
// the current Release version without changing the workload cluster.
func (s *ClusterService) planNaaS(ctx context.Context, row *model.Cluster, desired *cloudv1.ClusterSpec, plan *cloudv1.Plan) ([]string, error) {
	current, err := reconcilecluster.SpecOf(row)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	if current.GetNaas() == desired.GetNaas() {
		return nil, nil
	}
	name := crossplaneaws.HelmReleaseObjectName(crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name), "capsule")
	namespace := kube.TenantNamespace(row.TenantID)
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	version := "capsule-release=absent"
	if err := s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, live); err == nil {
		version = "capsule-release=" + string(live.GetUID()) + "/" + live.GetResourceVersion()
	} else if !apierrors.IsNotFound(err) {
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the managed Capsule installation for plan review")
	}
	action := cloudv1.PlannedAction_PLANNED_ACTION_CREATE
	detail := "Enable NaaS: install the platform-managed Capsule Helm installation in capsule-system. Existing cert-manager must already be ready; this flag does not install cert-manager."
	if !desired.GetNaas() {
		action = cloudv1.PlannedAction_PLANNED_ACTION_DELETE
		detail = "Disable NaaS: remove the platform-managed Capsule Helm installation from capsule-system. Capsule admission enforcement stops. Tenant workloads, Capsule custom resources and CRDs may remain; inspect retained resources before uninstall. The externally provided cert-manager is not removed."
	}
	plan.Warnings = append(plan.Warnings, detail)
	if reconcilecluster.ReleaseMoving(row) {
		plan.Warnings = append(plan.Warnings, "The requested NaaS flag is stored immediately, but the Capsule installation change waits for the current platform release move to finish. A halted move requires its normal retry or fix before the NaaS change can apply.")
	}
	for _, change := range plan.Changes {
		if change.Kind == crossplaneaws.HelmReleaseGVK.Kind && change.Name == name && change.Namespace == namespace && change.Action == action {
			change.SpecPath = "spec.naas"
			change.Detail += "; " + detail
			return []string{version}, nil
		}
	}
	plan.Changes = append(plan.Changes, &cloudv1.PlannedChange{
		ApiVersion: crossplaneaws.HelmReleaseGVK.GroupVersion().String(), Kind: crossplaneaws.HelmReleaseGVK.Kind,
		Name: name, Namespace: namespace, ExternalName: "capsule", SpecPath: "spec.naas", Action: action, Detail: detail,
	})
	return []string{version}, nil
}
