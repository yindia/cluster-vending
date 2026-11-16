package tenant

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/admin"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

const platformFreezeReadOnly = "A platform freeze is active. Tenant operators cannot unfreeze or bypass it; only a platform administrator can resume these resources."

// dashboardAccess also protects direct service calls, without the RPC interceptor.
func dashboardAccess(ctx context.Context, tenant string, write bool, req proto.Message) error {
	if err := auth.FromContext(ctx).Authorize(auth.Access{Scope: auth.ScopeTenant, Write: write}, tenant); err != nil {
		return service.ToConnect(err)
	}
	if err := service.Validate(req); err != nil {
		return err
	}
	if len(req.ProtoReflect().GetUnknown()) != 0 {
		return connect.NewError(connect.CodeInvalidArgument, "unknown tenant dashboard request fields are not supported")
	}
	return nil
}

type dashboardSelection struct {
	target                   *v1.FreezeTarget
	tenant, network, cluster string
}

// Resolve against the tenant-scoped repository BEFORE invoking the policy engine.
// An explicit platform target is forbidden even when its boolean value is false.
func (s *TenantService) dashboardTarget(ctx context.Context, tenant string, requested *v1.FreezeTarget) (dashboardSelection, error) {
	selected := dashboardSelection{tenant: tenant, target: &v1.FreezeTarget{Scope: &v1.FreezeTarget_TenantId{TenantId: tenant}}}
	if s.deps.Store == nil {
		return selected, connect.NewError(connect.CodeUnavailable, "tenant resource store is unavailable")
	}
	if requested == nil {
		_, err := s.deps.Store.Tenants().Get(ctx, tenant)
		return selected, dashboardLookupError(err)
	}
	switch target := requested.GetScope().(type) {
	case *v1.FreezeTarget_TenantId:
		if target.TenantId != tenant {
			return selected, connect.NewError(connect.CodePermissionDenied, "a tenant dashboard cannot select another tenant")
		}
		_, err := s.deps.Store.Tenants().Get(ctx, tenant)
		if err != nil {
			return selected, dashboardLookupError(err)
		}
	case *v1.FreezeTarget_NetworkId:
		row, err := s.deps.Store.Networks().Get(ctx, tenant, target.NetworkId)
		if err != nil {
			return selected, dashboardLookupError(err)
		}
		if row == nil || row.TenantID != tenant || row.ID != target.NetworkId {
			return selected, dashboardLookupError(repository.ErrNotFound)
		}
		selected.network = row.ID
	case *v1.FreezeTarget_ClusterId:
		row, err := s.deps.Store.Clusters().Get(ctx, tenant, target.ClusterId)
		if err != nil {
			return selected, dashboardLookupError(err)
		}
		if row == nil || row.TenantID != tenant || row.ID != target.ClusterId {
			return selected, dashboardLookupError(repository.ErrNotFound)
		}
		selected.cluster, selected.network = row.ID, row.NetworkID
	case *v1.FreezeTarget_Platform:
		return selected, connect.NewError(connect.CodePermissionDenied, "platform targeting is not available through TenantService")
	default:
		return selected, connect.NewError(connect.CodeInvalidArgument, "target must select this tenant or one of its resources")
	}
	selected.target = proto.Clone(requested).(*v1.FreezeTarget)
	return selected, nil
}

func dashboardLookupError(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, "resource not found in the requested tenant")
	}
	return service.ToConnect(err)
}

// The scoped admin reader already counts only the selected resources. Rebuild
// the public status as an allowlist: never expose global scope/exception lists.
func dashboardSafeStatus(in *v1.FreezeStatus, selected dashboardSelection) *v1.FreezeStatus {
	out := &v1.FreezeStatus{Target: proto.Clone(selected.target).(*v1.FreezeTarget), Phase: in.GetPhase(), Revision: in.GetRevision(),
		Message: in.GetMessage(), Clusters: in.GetClusters(), Networks: in.GetNetworks(), TotalResources: in.GetTotalResources(),
		PausedResources: in.GetPausedResources(), NewlyPausedResources: in.GetNewlyPausedResources(), PlatformFrozen: in.GetPlatformFrozen()}
	if frozen := in.GetFreeze(); frozen != nil {
		out.Freeze = proto.Clone(frozen).(*v1.Freeze)
		own := frozen.Scope == "tenant "+selected.tenant || (selected.network != "" && frozen.Scope == "network "+selected.network) || (selected.cluster != "" && frozen.Scope == "cluster "+selected.cluster)
		if !own || out.PlatformFrozen {
			out.Freeze = &v1.Freeze{Scope: "tenant " + selected.tenant, LetThrough: frozen.GetLetThrough(), Reason: "The selected tenant resources are frozen."}
		}
	}
	if out.PlatformFrozen {
		out.Message = platformFreezeReadOnly
		if out.Freeze != nil {
			out.Freeze.Scope, out.Freeze.Reason = "platform", platformFreezeReadOnly
		}
	}
	return out
}

func (s *TenantService) Freeze(ctx context.Context, req *v1.TenantFreezeRequest) (*v1.FreezeResponse, error) {
	if err := dashboardAccess(ctx, req.GetTenantId(), true, req); err != nil {
		return nil, err
	}
	selected, err := s.dashboardTarget(ctx, req.GetTenantId(), req.GetTarget())
	if err != nil {
		return nil, err
	}
	if s.deps.Kube == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "management cluster is unavailable")
	}
	result, err := admin.NewService(s.deps).Freeze(ctx, &v1.FreezeRequest{Target: selected.target, Reason: req.GetReason(), Until: req.GetUntil()})
	if err != nil {
		return nil, err
	}
	status := dashboardSafeStatus(result.GetStatus(), selected)
	return &v1.FreezeResponse{Status: status, Revision: status.Revision, Phase: convert.FreezePhaseText(status.Phase), Message: status.Message,
		Clusters: status.Clusters, Networks: status.Networks, TotalResources: status.TotalResources, PausedResources: status.PausedResources}, nil
}

// A second check at the actual policy write closes the race between our
// ownership/read checks and the admin engine reading a renewed platform hold.
type dashboardUnfreezeClient struct {
	client.Client
	blocked bool
}

func (c *dashboardUnfreezeClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if hold, ok := obj.(*cloud.ReleaseHold); ok && hold.Name == reconcile.FreezeName && reconcile.GlobalFreezeActive(hold, time.Now()) {
		c.blocked = true
		return connect.NewError(connect.CodeFailedPrecondition, platformFreezeReadOnly)
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (s *TenantService) Unfreeze(ctx context.Context, req *v1.TenantUnfreezeRequest) (*v1.UnfreezeResponse, error) {
	if err := dashboardAccess(ctx, req.GetTenantId(), true, req); err != nil {
		return nil, err
	}
	selected, err := s.dashboardTarget(ctx, req.GetTenantId(), req.GetTarget())
	if err != nil {
		return nil, err
	}
	if s.deps.Kube == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "management cluster is unavailable")
	}
	hold := &cloud.ReleaseHold{}
	if err := s.deps.Reader().Get(ctx, client.ObjectKey{Name: reconcile.FreezeName}, hold); err != nil && !apierrors.IsNotFound(err) {
		return nil, service.ToConnect(err)
	}
	if reconcile.GlobalFreezeActive(hold, time.Now()) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, platformFreezeReadOnly)
	}
	guard := &dashboardUnfreezeClient{Client: s.deps.Kube}
	deps := s.deps
	deps.Kube = guard
	result, err := admin.NewService(deps).Unfreeze(ctx, &v1.UnfreezeRequest{Target: selected.target})
	if guard.blocked {
		return nil, connect.NewError(connect.CodeFailedPrecondition, platformFreezeReadOnly)
	}
	if err != nil {
		return nil, err
	}
	return &v1.UnfreezeResponse{Note: "Unfreeze requested for the selected tenant scope; the controller resumes it asynchronously.", Revision: result.GetRevision(), Status: dashboardSafeStatus(result.GetStatus(), selected)}, nil
}

func (s *TenantService) GetFreeze(ctx context.Context, req *v1.TenantGetFreezeRequest) (*v1.GetFreezeResponse, error) {
	if err := dashboardAccess(ctx, req.GetTenantId(), false, req); err != nil {
		return nil, err
	}
	selected, err := s.dashboardTarget(ctx, req.GetTenantId(), req.GetTarget())
	if err != nil {
		return nil, err
	}
	if s.deps.Kube == nil && s.deps.KubeReader == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "management cluster is unavailable")
	}
	view, err := admin.NewService(s.deps).GetFreeze(ctx, &v1.GetFreezeRequest{Target: selected.target})
	if err != nil {
		return nil, err
	}
	status := dashboardSafeStatus(view.GetStatus(), selected)
	return &v1.GetFreezeResponse{Status: status, Freeze: status.Freeze, Revision: status.Revision, Phase: convert.FreezePhaseText(status.Phase), Message: status.Message,
		PlatformFrozen: status.PlatformFrozen, Clusters: status.Clusters, Networks: status.Networks, TotalResources: status.TotalResources,
		PausedResources: status.PausedResources, NewlyPausedResources: status.NewlyPausedResources}, nil
}
