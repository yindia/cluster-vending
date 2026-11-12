package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
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
	for _, r := range records {
		if rendered[r.Kind+"/"+r.Name] || r.Kind == "Usage" || r.CarriedOver {
			continue
		}
		versions = append(versions, r.Kind+"/"+r.Name+"=recorded")
		change := &cloudv1.PlannedChange{ApiVersion: r.APIVersion, Kind: r.Kind, Name: r.Name,
			Action: cloudv1.PlannedAction_PLANNED_ACTION_DELETE, Detail: "no longer part of the cluster"}
		if r.Kind == reconcilecluster.KindNodeGroup {
			change.RequiresReplacement = true
			change.Detail = "replaced by " + name + "; deleted once the replacement is ready"
			change.WaitsFor = "node group " + name + " is ready"
		}
		plan.Changes = append(plan.Changes, change)
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
	versions := []string{}
	rendered := map[string]bool{}
	for _, obj := range crossplaneaws.ClusterResources(render) {
		body, err := kube.ApplyObject(obj, s.deps.Kube.Scheme())
		if err != nil {
			return nil, nil, connect.NewError(connect.CodeInternal, err.Error())
		}
		rendered[body.GetKind()+"/"+body.GetName()] = true
		if body.GetKind() == "Usage" {
			continue
		}
		if body.GetKind() == "NodeGroup" {
			crossplaneaws.DesiredSizeAtCreateOnly(body)
		}
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(body.GroupVersionKind())
		err = s.deps.Reader().Get(ctx, client.ObjectKeyFromObject(body), live)
		if apierrors.IsNotFound(err) {
			versions = append(versions, body.GetKind()+"/"+body.GetName()+"=absent")
			plan.Changes = append(plan.Changes, &cloudv1.PlannedChange{ApiVersion: body.GetAPIVersion(), Kind: body.GetKind(),
				Name: body.GetName(), Action: cloudv1.PlannedAction_PLANNED_ACTION_CREATE,
				RequiresReplacement: replacing && body.GetKind() == "NodeGroup", Detail: corecluster.CreateDetail(body, replacing)})
			continue
		}
		if err != nil {
			return nil, nil, connect.NewError(connect.CodeUnavailable, fmt.Sprintf("cannot read %s %s: %v", body.GetKind(), body.GetName(), err))
		}
		versions = append(versions, body.GetKind()+"/"+body.GetName()+"="+live.GetResourceVersion())
		dry, err := kube.DryRunApply(ctx, s.deps.Kube, body)
		if err != nil {
			return nil, nil, connect.NewError(connect.CodeFailedPrecondition, "the update renders a resource the management cluster refuses: "+err.Error())
		}
		plan.Changes = append(plan.Changes, corecluster.FieldChanges(live, dry)...)
	}
	return versions, rendered, nil
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
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if err != nil {
		return "", err
	}
	sort.Strings(versions)
	h := sha256.New()
	h.Write(raw)
	h.Write([]byte(strings.Join(versions, "\n")))
	return hex.EncodeToString(h.Sum(nil)), nil
}
