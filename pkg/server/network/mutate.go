package network

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// Update mask paths UpdateNetwork accepts. The layout, region and account
// cannot change yet.
const (
	pathDisplayName = "display_name"
	pathLabels      = "labels"
	pathFlowLogs    = "spec.flow_logs"
)

// updatable are the update mask paths, for error messages.
var updatable = strings.Join([]string{pathDisplayName, pathLabels, pathFlowLogs}, ", ")

// changes is what an update changed that needs the resources touched.
type changes struct{ labels, flowLogs bool }

// UpdateNetwork changes the fields named in update_mask: display_name,
// labels and spec.flow_logs. Labels are the AWS tags of every resource: an
// update re-tags (an imported network's resources keep their other tags).
// Flow logs are turned on or off and their retention changed by rendering
// the network again.
func (s *NetworkService) UpdateNetwork(ctx context.Context, req *cloudv1.UpdateNetworkRequest) (*cloudv1.UpdateNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	in := req.GetNetwork()
	row, err := s.get(ctx, req.GetTenantId(), in.GetId())
	if err != nil {
		return nil, err
	}
	if err := checkMutable(row, in.GetEtag()); err != nil {
		return nil, err
	}
	spec, err := specOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "update_mask is empty; updatable: "+updatable)
	}
	labelsBefore := maps.Clone(row.Labels)
	changed, err := applyMask(row, spec, in, paths)
	if err != nil {
		return nil, err
	}
	// A taken-over network's objects come from the discovered VPC, not from
	// the spec: it is never rendered again (that would create a second
	// network). Its labels are set on the objects it has.
	imported := storedImport(row) != nil
	if imported && changed.flowLogs {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			"spec.flow_logs cannot be set on an imported network: it keeps the flow logs it was imported with")
	}
	tagsChanged := changed.labels || changed.flowLogs
	if row.Spec, err = encode(spec); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	if row.Input, err = updatedInput(row, in, req.GetUpdateMask().GetPaths()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row.Generation++
	if !tagsChanged && row.AppliedGeneration == row.Generation-1 {
		row.AppliedGeneration = row.Generation // nothing to apply for this change
	}
	if tagsChanged && row.State == stateReady {
		row.State = stateUpdating // the loop records the status until it is READY again
	}
	if err := s.deps.Store.Networks().Update(ctx, row); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, connect.NewError(connect.CodeAborted, "the network changed; get it again and retry with its etag")
		}
		return nil, service.ToConnect(err)
	}
	switch {
	case imported && changed.labels:
		if err := s.retag(ctx, row, labelsBefore); err != nil {
			return nil, err
		}
	case tagsChanged:
		if err := s.reapply(ctx, row, spec); err != nil {
			return nil, err
		}
	}
	n, err := s.respond(ctx, row)
	return &cloudv1.UpdateNetworkResponse{Network: n}, err
}

// checkMutable refuses updates of deleting networks and stale etags.
func checkMutable(row *model.Network, etag string) error {
	if row.State == stateDeleting {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("network %s is being deleted", row.ID))
	}
	if row.State == stateImported || row.State == stateBlocked {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("network %s is imported read-only; take it over first", row.ID))
	}
	if etag != "" && etag != row.ETag {
		return connect.NewError(connect.CodeAborted, "etag mismatch: the network changed; get it again and retry")
	}
	return nil
}

// applyMask copies the masked fields of in onto row and spec, and checks
// the result against the contract. It reports what must be applied to the
// resources: labels (they are the AWS tags) and flow logs.
func applyMask(row *model.Network, spec *cloudv1.NetworkSpec, in *cloudv1.Network, paths []string) (changes, error) {
	var changed changes
	for _, p := range paths {
		switch p {
		case pathDisplayName:
			row.DisplayName = in.GetDisplayName()
		case pathLabels:
			row.Labels = model.Labels(in.GetLabels())
			// Always re-applied, even unchanged: that also adds tags the
			// platform introduced after the network was created.
			changed.labels = true
		case pathFlowLogs:
			next := resolvedFlowLogs(in.GetSpec().GetFlowLogs())
			if err := flowLogsChangeable(spec.GetFlowLogs(), next); err != nil {
				return changes{}, err
			}
			spec.FlowLogs, changed.flowLogs = next, true
		default:
			return changes{}, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(
				"update_mask path %q cannot be updated; updatable: %s", p, updatable))
		}
	}
	merged := &cloudv1.Network{Name: row.Name, DisplayName: row.DisplayName, Labels: row.Labels, Spec: spec}
	if err := protovalidate.Validate(merged); err != nil {
		return changes{}, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return changed, service.CheckAWSLabels(row.Labels)
}

// flowLogsChangeable refuses what AWS cannot change on a flow log that
// exists: its traffic type and aggregation interval.
func flowLogsChangeable(before, after *cloudv1.NetworkFlowLogs) error {
	if !before.GetEnabled() || !after.GetEnabled() {
		return nil // turned on or off: created or deleted
	}
	if before.GetTrafficType() != after.GetTrafficType() ||
		before.GetAggregationIntervalSeconds() != after.GetAggregationIntervalSeconds() {
		return connect.NewError(connect.CodeFailedPrecondition,
			"the traffic type and aggregation interval of existing flow logs cannot be changed; turn flow logs off, then on with the new settings")
	}
	return nil
}

// updatedInput is the stored input with the masked fields of in copied
// over, so input stays the network as the client asked for it (layout as
// first sent, defaults unresolved). A row stored before inputs were kept
// has none and keeps none: its original request is unknown.
func updatedInput(row *model.Network, in *cloudv1.Network, paths []string) (model.JSON, error) {
	if len(row.Input) == 0 {
		return row.Input, nil
	}
	input := &cloudv1.Network{}
	if err := lenient.Unmarshal(row.Input, input); err != nil {
		return nil, fmt.Errorf("network %s: stored input: %w", row.ID, err)
	}
	for _, p := range paths {
		switch p {
		case pathDisplayName:
			input.DisplayName = in.GetDisplayName()
		case pathLabels:
			input.Labels = in.GetLabels()
		case pathFlowLogs:
			if input.Spec == nil {
				input.Spec = &cloudv1.NetworkSpec{}
			}
			input.Spec.FlowLogs = in.GetSpec().GetFlowLogs()
		}
	}
	return encode(input)
}

// reapply renders the stored network again and applies it.
func (s *NetworkService) reapply(ctx context.Context, row *model.Network, spec *cloudv1.NetworkSpec) error {
	account, layout, err := s.plan(ctx, row.TenantID, spec)
	if err != nil {
		return err
	}
	return s.apply(ctx, row, account, newResolved(row, spec, account, layout))
}

// DeleteNetwork deletes the network's resources; the network is removed
// once they are gone (until then it is DELETING). It fails while clusters
// use the network.
func (s *NetworkService) DeleteNetwork(ctx context.Context, req *cloudv1.DeleteNetworkRequest) (*cloudv1.DeleteNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, err
	}
	if etag := req.GetEtag(); etag != "" && etag != row.ETag {
		return nil, connect.NewError(connect.CodeAborted, "etag mismatch: the network changed; get it again and retry")
	}
	if err := s.refuseInUse(ctx, row); err != nil {
		return nil, err
	}
	if row.State != stateDeleting {
		row.State = stateDeleting
		if err := s.deps.Store.Networks().Update(ctx, row); err != nil {
			return nil, service.ToConnect(err)
		}
	}
	n, err := s.respond(ctx, row)
	if err != nil {
		return nil, err
	}
	// Deletes the first tier now; the background loop moves it on, tier by
	// tier, until the network is gone.
	if err := s.finalize(ctx, row); err != nil {
		return nil, err
	}
	s.logger().InfoContext(ctx, "network deleting", "tenant_id", row.TenantID, "network_id", row.ID, "name", row.Name)
	return &cloudv1.DeleteNetworkResponse{Network: n}, nil
}

// refuseInUse fails while clusters use the network, also clusters still
// being deleted: the background loop removes them once their resources are
// confirmed gone and the subnets are untagged.
func (s *NetworkService) refuseInUse(ctx context.Context, row *model.Network) error {
	clusters, err := s.deps.Store.Clusters().ListByNetwork(ctx, row.ID)
	if err != nil {
		return service.ToConnect(err)
	}
	var using, deleting []string
	for _, c := range clusters {
		if c.State == clusterStateDeleting {
			deleting = append(deleting, fmt.Sprintf("%s (%s)", c.Name, c.ID))
		} else {
			using = append(using, fmt.Sprintf("%s (%s)", c.Name, c.ID))
		}
	}
	switch {
	case len(using) > 0:
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("network %s is used by clusters %s; delete them first", row.ID, strings.Join(using, ", ")))
	case len(deleting) > 0:
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("network %s is used by clusters still being deleted: %s; retry once they are gone (cluster get --wait)",
				row.ID, strings.Join(deleting, ", ")))
	}
	return nil
}

// clusterStateDeleting is a cluster row's state while it is deleted.
const clusterStateDeleting = "CLUSTER_STATE_DELETING"
