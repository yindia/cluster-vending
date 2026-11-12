package network

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// stored is the API view of a network from the database alone: its spec,
// the status last recorded for it (remember) and the clusters using it
// now. GetNetwork and ListNetworks return it; nothing is read from the
// management cluster.
func (s *NetworkService) stored(ctx context.Context, row *model.Network) (*cloudv1.Network, error) {
	spec, err := specOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	st := &cloudv1.NetworkStatus{}
	if len(row.Status) > 0 {
		_ = lenient.Unmarshal(row.Status, st) // written by remember; unreadable: start empty
	}
	if state := storedState(row.State, st.GetState()); state != st.GetState() {
		// The row moved on since the status was recorded (a change, a
		// delete or a takeover was stored): its message is about the past.
		st.State, st.Message = state, ""
	}
	clusters, err := s.deps.Store.Clusters().ListByNetwork(ctx, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	st.ClusterIds = nil
	for _, c := range clusters {
		st.ClusterIds = append(st.ClusterIds, c.Name)
	}
	return toProto(row, spec, st), nil
}

// storedState is the state a stored network has: the recorded one, unless
// the row's lifecycle state says something happened since.
func storedState(rowState string, recorded cloudv1.NetworkState) cloudv1.NetworkState {
	own := cloudv1.NetworkState(cloudv1.NetworkState_value[rowState])
	switch {
	case recorded == cloudv1.NetworkState_NETWORK_STATE_UNSPECIFIED, rowState == stateDeleting, rowState == stateReady:
		// READY is stored only once the loop saw every resource ready; it
		// holds even if recording that pass's status failed.
		return own
	case recorded == cloudv1.NetworkState_NETWORK_STATE_READY && (rowState == stateProvisioning || rowState == stateUpdating):
		return own
	case recorded == cloudv1.NetworkState_NETWORK_STATE_IMPORTED && rowState != stateImported:
		return own
	default:
		return recorded
	}
}

// remember records a network's status as observed now, for GetNetwork and
// ListNetworks. It writes the status column only: the etag does not
// change. A network that is gone has nothing to record, and one that a
// call changed since row was read keeps what that call stored.
func (s *NetworkService) remember(ctx context.Context, row *model.Network, st *cloudv1.NetworkStatus) error {
	raw, err := encode(st)
	if err != nil {
		return err
	}
	if string(raw) == string(row.Status) {
		return nil
	}
	err = s.deps.Store.Networks().SetStatus(ctx, row.TenantID, row.ID, row.ETag, raw)
	if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrConflict) {
		return nil // gone, or changed since it was read: the next pass records it
	}
	if err == nil {
		row.Status = raw
	}
	return err
}

// respond is the answer of a call that changed the network: its view with
// the live status, which is recorded on the way.
func (s *NetworkService) respond(ctx context.Context, row *model.Network) (*cloudv1.Network, error) {
	n, err := s.view(ctx, row)
	if err != nil {
		return nil, err
	}
	if err := s.remember(ctx, row, n.GetStatus()); err != nil {
		s.logger().WarnContext(ctx, "record network status", "network_id", row.ID, "error", err)
	}
	return n, nil
}

// GetNetworkStatus returns one network with its status read live from the
// management cluster. Like every read it changes nothing.
func (s *NetworkService) GetNetworkStatus(ctx context.Context, req *cloudv1.GetNetworkStatusRequest) (*cloudv1.GetNetworkStatusResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, err
	}
	n, err := s.view(ctx, row)
	return &cloudv1.GetNetworkStatusResponse{Network: n}, err
}

// settle records what the loop observed of a network that is not being
// deleted: its status, and READY as its stored state once every resource
// is ready and its generation is applied, so the loop leaves it alone
// until it changes.
func (s *NetworkService) settle(ctx context.Context, row *model.Network) error {
	n, err := s.view(ctx, row)
	if err != nil {
		return err
	}
	st := n.GetStatus()
	converging := row.State == stateProvisioning || row.State == stateUpdating
	if !converging || st.GetState() != cloudv1.NetworkState_NETWORK_STATE_READY || row.AppliedGeneration < row.Generation {
		return s.remember(ctx, row, st)
	}
	// READY and its status in one write: once READY the loop does not come
	// back, so the recorded status must not be left behind.
	raw, err := encode(st)
	if err != nil {
		return err
	}
	before, status := row.State, row.Status
	row.State, row.Status = stateReady, raw
	if err := s.deps.Store.Networks().Update(ctx, row); err != nil {
		row.State, row.Status = before, status
		if errors.Is(err, repository.ErrConflict) {
			return nil // an API call changed the row; the next pass sees it
		}
		return err
	}
	return nil
}
