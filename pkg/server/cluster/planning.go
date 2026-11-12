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

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// apiFields maps a managed resource's forProvider field (by prefix) to the
// API field it is rendered from, so a plan names what the client set.
var apiFields = map[string]map[string]string{
	"Cluster": {
		"version":                         "spec.control_plane.kubernetes_version",
		"vpcConfig.publicAccessCidrs":     "spec.control_plane.endpoint_access.public_access_cidrs",
		"vpcConfig.endpointPrivateAccess": "spec.control_plane.endpoint_access.endpoint_private_access",
		"vpcConfig.endpointPublicAccess":  "spec.control_plane.endpoint_access.endpoint_public_access",
		"vpcConfig.securityGroupIds":      "spec.aws.additional_security_group_ids",
		"encryptionConfig":                "spec.control_plane.secrets_encryption",
		"enabledClusterLogTypes":          "spec.aws.enabled_cluster_log_types",
		"upgradePolicy":                   "spec.aws.upgrade_policy.support_type",
		"accessConfig.authenticationMode": "spec.aws.access_config.authentication_mode",
		"tags":                            "labels",
	},
	"NodeGroup": {
		"version":               "spec.control_plane.kubernetes_version",
		"scalingConfig.minSize": "spec.default_node_pool.scaling_config.min_size",
		"scalingConfig.maxSize": "spec.default_node_pool.scaling_config.max_size",
		"labels":                "spec.default_node_pool.labels",
		"taint":                 "spec.default_node_pool.taints",
		"instanceTypes":         "spec.default_node_pool.instance_types",
		"capacityType":          "spec.default_node_pool.capacity_type",
		"diskSize":              "spec.default_node_pool.disk_size_gb",
		"amiType":               "spec.aws.default_node_pool.ami_type",
		"launchTemplate":        "spec.aws.default_node_pool.launch_template",
		"tags":                  "labels",
	},
	"Addon": {"addonVersion": "spec.aws.addons", "configurationValues": "spec.aws.addons", "tags": "labels"},
	"Group": {"retentionInDays": "spec.aws.control_plane_log_group.retention_days", "tags": "labels"},
}

// apiField is the API field of a kind's forProvider path, else the path
// itself ("forProvider.<path>").
func apiField(kind, path string) string {
	for p := path; ; {
		if f, ok := apiFields[kind][p]; ok {
			return f
		}
		i := strings.LastIndex(p, ".")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	if strings.HasPrefix(path, "tags.") || path == "tags" {
		return "labels"
	}
	return "forProvider." + path
}

// planUpdate computes what applying row with spec (both already carrying
// the update, not stored) would do, changing nothing: the same render as
// UpdateCluster, each object server-side applied as a dry run and compared
// with the live one, plus the deletions Prune and the node group
// retirement would make. Usages (they only order deletions) are not
// listed. The plan's ID covers req and the live objects' versions.
func (s *ClusterService) planUpdate(ctx context.Context, req *cloudv1.UpdateClusterRequest, row *model.Cluster,
	spec *cloudv1.ClusterSpec, t target,
) (*cloudv1.Plan, error) {
	render := s.renderPlan(spec, row, t)
	name, retiring, err := s.nodeGroupName(ctx, row, render)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the cluster's node group")
	}
	render.NodeGroupName = name
	if err := s.fillLive(ctx, row, &render); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the cluster's resources")
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	plan := &cloudv1.Plan{Applicable: true}
	pending := render.Pending()
	if len(pending) > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: planned and applied once what they need exists "+
			"(the encryption key, the cluster's issuer or security group); this plan does not list their changes",
			strings.Join(pending, ", ")))
	}
	versions := []string{}
	rendered := map[string]bool{}
	replacing := len(retiring) > 0
	for _, obj := range crossplaneaws.ClusterResources(render) {
		body, err := kube.ApplyObject(obj, s.deps.Kube.Scheme())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error())
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
				RequiresReplacement: replacing && body.GetKind() == "NodeGroup", Detail: createDetail(body, replacing)})
			continue
		}
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Sprintf("cannot read %s %s: %v", body.GetKind(), body.GetName(), err))
		}
		versions = append(versions, body.GetKind()+"/"+body.GetName()+"="+live.GetResourceVersion())
		dry, err := kube.DryRunApply(ctx, s.deps.Kube, body)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, "the update renders a resource the management cluster refuses: "+err.Error())
		}
		plan.Changes = append(plan.Changes, fieldChanges(live, dry)...)
	}
	for _, r := range records {
		if rendered[r.Kind+"/"+r.Name] || r.Kind == "Usage" || slices.Contains(pending, r.Kind) {
			continue
		}
		versions = append(versions, r.Kind+"/"+r.Name+"=recorded")
		change := &cloudv1.PlannedChange{ApiVersion: r.APIVersion, Kind: r.Kind, Name: r.Name,
			Action: cloudv1.PlannedAction_PLANNED_ACTION_DELETE, Detail: "no longer part of the cluster"}
		if r.Kind == kindNodeGroup {
			change.RequiresReplacement = true
			change.Detail = "replaced by " + name + "; deleted once the replacement is ready"
		}
		plan.Changes = append(plan.Changes, change)
	}
	if change := s.encryptionChange(row, spec, render, plan.Changes); change != nil {
		plan.Changes = append(plan.Changes, change)
	}
	for _, c := range plan.Changes {
		switch c.GetAction() {
		case cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE:
			plan.Applicable = false
		case cloudv1.PlannedAction_PLANNED_ACTION_DISRUPTIVE:
			if c.GetKind() == "Cluster" && strings.HasSuffix(c.GetSpecPath(), "kubernetes_version") {
				plan.Warnings = append(plan.Warnings, "Kubernetes upgrade: the control plane is upgraded first (about 10 minutes); "+
					"node groups and add-ons follow once it reports the new version, nodes are replaced one by one")
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

// encryptionChange is the refusal of an update that turns the secrets
// encryption off or moves it to another key, unless the field comparison
// already lists one: UpdateCluster refuses such an update whatever the
// objects' comparison shows.
func (s *ClusterService) encryptionChange(row *model.Cluster, spec *cloudv1.ClusterSpec, render crossplaneaws.ClusterPlan,
	changes []*cloudv1.PlannedChange,
) *cloudv1.PlannedChange {
	stored, err := specOf(row)
	if err != nil || encryptionKept(encryption(stored), encryption(spec)) {
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

func createDetail(body *unstructured.Unstructured, replacing bool) string {
	if replacing && body.GetKind() == "NodeGroup" {
		return "created to replace the current node group (a setting EKS cannot change in place changed)"
	}
	return "does not exist yet"
}

// fieldChanges compares a live object with its dry-run apply, field by
// field of spec.forProvider and spec.initProvider, and with the cloud's
// state (status.atProvider): a field the update leaves alone but the cloud
// has differently was changed outside the platform, and is set back.
func fieldChanges(live, dry *unstructured.Unstructured) []*cloudv1.PlannedChange {
	var out []*cloudv1.PlannedChange
	change := func(path string, action cloudv1.PlannedAction, detail, diff string) {
		out = append(out, &cloudv1.PlannedChange{ApiVersion: dry.GetAPIVersion(), Kind: dry.GetKind(), Name: dry.GetName(),
			ExternalName: live.GetAnnotations()[crossplaneaws.ExternalNameAnnotation], SpecPath: apiField(dry.GetKind(), path),
			Action: action, Detail: detail, UnifiedDiff: diff})
	}
	header := fmt.Sprintf("--- %s/%s (live)\n+++ %s/%s (planned)\n", dry.GetKind(), dry.GetName(), dry.GetKind(), dry.GetName())
	was := fields(live, "spec", "forProvider")
	will := fields(dry, "spec", "forProvider")
	cloud := fields(live, "status", "atProvider")
	for _, path := range sortedKeys(was, will) {
		before, after := crossplaneaws.FieldString(was[path]), crossplaneaws.FieldString(will[path])
		if before != after {
			change(path, actionFor(dry.GetKind(), path, was[path] == nil), fmt.Sprintf("forProvider.%s: %s -> %s", path, before, after),
				fmt.Sprintf("%s-forProvider.%s: %s\n+forProvider.%s: %s\n", header, path, before, path, after))
			continue
		}
		if actual, ok := cloud[path]; ok && isScalar(actual) && isScalar(will[path]) && crossplaneaws.FieldString(actual) != after {
			change(path, actionFor(dry.GetKind(), path, false),
				fmt.Sprintf("forProvider.%s: the cloud has %s (changed outside the platform), set back to %s", path,
					crossplaneaws.FieldString(actual), after), "")
		}
	}
	wasInit, willInit := fields(live, "spec", "initProvider"), fields(dry, "spec", "initProvider")
	for _, path := range sortedKeys(wasInit, willInit) {
		before, after := crossplaneaws.FieldString(wasInit[path]), crossplaneaws.FieldString(willInit[path])
		if before != after {
			change(path, cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE,
				fmt.Sprintf("initProvider.%s: %s -> %s (used at creation only: no effect on the existing resource)", path, before, after), "")
		}
	}
	return out
}

func fields(u *unstructured.Unstructured, path ...string) map[string]any {
	m, _, _ := unstructured.NestedMap(u.Object, path...)
	return crossplaneaws.FlattenFields(m)
}

func sortedKeys(maps ...map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, int64, float64, int:
		return true
	default:
		return false
	}
}

// actionFor is the planned action of changing a field of an existing
// resource; unset says the resource has no value for it yet. A field not
// classified is treated as immutable: refused rather than guessed. An
// add-only field (secrets encryption) can be set where there is none, and
// is refused otherwise.
func actionFor(kind, path string, unset bool) cloudv1.PlannedAction {
	class, ok := crossplaneaws.ClassifyField(kind, path)
	switch {
	case class == crossplaneaws.ChangeAddOnly && unset:
		return cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE
	case !ok, class == crossplaneaws.ChangeImmutable, class == crossplaneaws.ChangeAddOnly:
		return cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE
	case class == crossplaneaws.ChangeDisruptive:
		return cloudv1.PlannedAction_PLANNED_ACTION_DISRUPTIVE
	default:
		return cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE
	}
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
