package network

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"

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
	pathTags        = "spec.tags"
)

// UpdateNetwork changes the fields named in update_mask: display_name,
// labels and spec.tags. New tags are applied to every resource.
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
	tagsChanged, err := applyMask(row, spec, in, req.GetUpdateMask().GetPaths())
	if err != nil {
		return nil, err
	}
	if row.Spec, err = encode(spec); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	if row.Input, err = updatedInput(row, in, req.GetUpdateMask().GetPaths()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row.Generation++
	if err := s.deps.Store.Networks().Update(ctx, row); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, connect.NewError(connect.CodeAborted, "the network changed; get it again and retry with its etag")
		}
		return nil, service.ToConnect(err)
	}
	if tagsChanged {
		if err := s.reapply(ctx, row, spec); err != nil {
			return nil, err
		}
	}
	n, err := s.view(ctx, row)
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
// the result against the contract. It reports whether the tags changed.
func applyMask(row *model.Network, spec *cloudv1.NetworkSpec, in *cloudv1.Network, paths []string) (bool, error) {
	tagsChanged := false
	for _, p := range paths {
		switch p {
		case pathDisplayName:
			row.DisplayName = in.GetDisplayName()
		case pathLabels:
			row.Labels = model.Labels(in.GetLabels())
		case pathTags:
			// Always re-applied, even unchanged: that also adds tags the
			// platform introduced after the network was created.
			tagsChanged = true
			spec.Tags = in.GetSpec().GetTags()
		default:
			return false, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(
				"update_mask path %q cannot be updated; updatable: %s", p,
				strings.Join([]string{pathDisplayName, pathLabels, pathTags}, ", ")))
		}
	}
	merged := &cloudv1.Network{Name: row.Name, DisplayName: row.DisplayName, Labels: row.Labels, Spec: spec}
	if err := protovalidate.Validate(merged); err != nil {
		return false, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return tagsChanged, nil
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
	if err := protojson.Unmarshal(row.Input, input); err != nil {
		return nil, fmt.Errorf("network %s: stored input: %w", row.ID, err)
	}
	for _, p := range paths {
		switch p {
		case pathDisplayName:
			input.DisplayName = in.GetDisplayName()
		case pathLabels:
			input.Labels = in.GetLabels()
		case pathTags:
			if input.Spec == nil {
				input.Spec = &cloudv1.NetworkSpec{}
			}
			input.Spec.Tags = in.GetSpec().GetTags()
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
	if err := s.deleteResources(ctx, row.TenantID, row.ID); err != nil {
		s.logger().ErrorContext(ctx, "delete network resources", "network_id", row.ID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, "could not delete the network's resources")
	}
	n, err := s.view(ctx, row)
	if err != nil {
		return nil, err
	}
	if _, err := s.finalize(ctx, row); err != nil {
		return nil, err
	}
	s.logger().InfoContext(ctx, "network deleting", "tenant_id", row.TenantID, "network_id", row.ID, "name", row.Name)
	return &cloudv1.DeleteNetworkResponse{Network: n}, nil
}

func (s *NetworkService) refuseInUse(ctx context.Context, row *model.Network) error {
	clusters, err := s.deps.Store.Clusters().ListByNetwork(ctx, row.ID)
	if err != nil {
		return service.ToConnect(err)
	}
	if len(clusters) == 0 {
		return nil
	}
	names := make([]string, 0, len(clusters))
	for _, c := range clusters {
		names = append(names, c.Name)
	}
	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Sprintf("network %s is used by clusters %s; delete them first", row.ID, strings.Join(names, ", ")))
}
