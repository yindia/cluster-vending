package network

import (
	"context"
	"strings"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/planning"
	"github.com/yindia/cluster-vending/pkg/database/model"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func (s *NetworkService) planUpdate(ctx context.Context, req *v1.UpdateNetworkRequest, row *model.Network, spec *v1.NetworkSpec) (*v1.Plan, error) {
	plan := &v1.Plan{Applicable: true}
	var err error

	metadataOnly := true
	for _, path := range req.GetUpdateMask().GetPaths() {
		if path != pathTier && path != pathDisplayName {
			metadataOnly = false
		}
	}
	if metadataOnly {
		plan.Warnings = append(plan.Warnings, "platform metadata changes only; all clusters on the network inherit its tier")
		normalized := proto.CloneOf(req)
		normalized.ValidateOnly, normalized.PlanId = false, ""
		plan.PlanId, err = planning.ID(normalized, []string{"network=" + row.ID + "/" + row.ETag})
		return plan, err
	}
	objects, err := s.rec().RenderResources(ctx, row, spec)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	// The engine is cloud-neutral; field safety belongs to the AWS network adapter.
	policy := planning.Policy{
		Classify: func(kind schema.GroupVersionKind, path string, _ bool) v1.PlannedAction {
			if path == "tags" || strings.HasPrefix(path, "tags.") || kind.Kind == "Group" && path == "retentionInDays" {
				return v1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE
			}
			return v1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE
		},
		APIField: func(kind schema.GroupVersionKind, path string) string {
			if path == "tags" || strings.HasPrefix(path, "tags.") {
				return "labels"
			}
			if kind.Kind == "Group" && path == "retentionInDays" {
				return "spec.flow_logs.retention_days"
			}
			return "forProvider." + path
		}}
	snapshot := planning.Snapshot{Client: s.deps.Kube, Reader: s.deps.Reader(), Policy: policy}
	versions, rendered, err := snapshot.Render(ctx, objects, plan)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	entries := make([]planning.Entry, 0, len(records))
	for _, record := range records {
		entries = append(entries, planning.Entry{Ref: managed.Ref{APIVersion: record.APIVersion, Kind: record.Kind, Namespace: record.Namespace, Name: record.Name, ExternalName: record.ExternalName}, Retained: record.CarriedOver})
	}
	removed, err := snapshot.Removals(ctx, entries, rendered, plan)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	versions = append(versions, removed...)
	versions = append(versions, "network="+row.ID+"/"+row.ETag)
	for _, change := range plan.Changes {
		if change.Action == v1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE {
			plan.Applicable = false
		}
	}
	for _, path := range req.GetUpdateMask().GetPaths() {
		if path == pathTier {
			plan.Warnings = append(plan.Warnings, "all clusters on this network inherit its updated tier; queued rollouts will be replanned")
		}
	}
	normalized := proto.CloneOf(req)
	normalized.ValidateOnly, normalized.PlanId = false, ""
	plan.PlanId, err = planning.ID(normalized, versions)
	return plan, err
}
