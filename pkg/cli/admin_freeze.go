package cli

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func freezeStatusRequest(o Options) *cloudv1.GetFreezeRequest {
	return &cloudv1.GetFreezeRequest{Target: convert.FreezeTarget(nil, o.Tenant, o.PauseNetwork, o.Cluster), TenantId: o.Tenant, NetworkId: o.PauseNetwork, ClusterId: o.Cluster}
}

func freezeTarget(o Options) error {
	if o.Channel != "" || o.Tier != "" {
		return fmt.Errorf("freeze takes --platform, --tenant, --network or --cluster; --channel and --tier are not supported")
	}
	count := 0
	for _, set := range []bool{o.Platform, o.Cluster != "", o.PauseNetwork != "", o.Tenant != ""} {
		if set {
			count++
		}
	}
	if count > 1 {
		return fmt.Errorf("choose only one of --platform, --tenant, --network and --cluster; not both or all")
	}
	return nil
}

func AdminFreeze(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := freezeTarget(o); err != nil {
		return nil, err
	}
	until, err := parseUntil(o.Until)
	if err != nil {
		return nil, err
	}
	selected := freezeStatusRequest(o)
	// Mirror the legacy IDs so an older server cannot mistake a scoped request
	// for platform scope if it does not recognize the new target field.
	response, err := call(ctx, &cloudv1.FreezeRequest{Target: selected.Target, TenantId: o.Tenant, NetworkId: o.PauseNetwork, ClusterId: o.Cluster, Until: until, Reason: o.Reason, ResetExceptions: o.ResetExceptions}, c.Admin.Freeze)
	if err != nil {
		return nil, err
	}
	result := response.(*cloudv1.FreezeResponse)
	status := result.GetStatus()
	if status == nil {
		status = convert.FreezeStatus(&cloudv1.GetFreezeResponse{Phase: result.Phase, Revision: result.Revision, Message: result.Message, Clusters: result.Clusters, Networks: result.Networks, TotalResources: result.TotalResources, PausedResources: result.PausedResources}, selected.Target)
	}
	if o.Wait {
		status, err = waitFreeze(ctx, c, o, selected, status.Revision, true)
		if err != nil {
			return nil, err
		}
	}
	return printFreezeStatus(o, status)
}

func AdminUnfreeze(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := freezeTarget(o); err != nil {
		return nil, err
	}
	selected := freezeStatusRequest(o)
	response, err := call(ctx, &cloudv1.UnfreezeRequest{Target: selected.Target, TenantId: o.Tenant, NetworkId: o.PauseNetwork, ClusterId: o.Cluster}, c.Admin.Unfreeze)
	if err != nil {
		return nil, err
	}
	result := response.(*cloudv1.UnfreezeResponse)
	status := result.GetStatus()
	if o.Wait {
		status, err = waitFreeze(ctx, c, o, selected, result.Revision, false)
		if err != nil {
			return nil, err
		}
	} else if status == nil {
		view, readErr := call(ctx, selected, c.Admin.GetFreeze)
		if readErr != nil {
			return nil, readErr
		}
		status = convert.FreezeStatus(view.(*cloudv1.GetFreezeResponse), selected.Target)
	}
	return printFreezeStatus(o, status)
}

func waitFreeze(ctx context.Context, c Clients, o Options, selected *cloudv1.GetFreezeRequest, revision string, freeze bool) (*cloudv1.FreezeStatus, error) {
	return waitFreezeStatus(ctx, o, revision, freeze, "admin freeze status", func(ctx context.Context) (*cloudv1.FreezeStatus, error) {
		response, err := call(ctx, selected, c.Admin.GetFreeze)
		if err != nil {
			return nil, err
		}
		return convert.FreezeStatus(response.(*cloudv1.GetFreezeResponse), selected.Target), nil
	})
}

func waitFreezeStatus(ctx context.Context, o Options, revision string, freeze bool, inspect string, read func(context.Context) (*cloudv1.FreezeStatus, error)) (*cloudv1.FreezeStatus, error) {
	var confirmed *cloudv1.FreezeStatus
	want := cloudv1.FreezePhase_FREEZE_PHASE_UNFROZEN
	action := "unfreeze"
	if freeze {
		want = cloudv1.FreezePhase_FREEZE_PHASE_FROZEN
		action = "freeze"
	}
	err := waitUntil(ctx, o, action, false, func(ctx context.Context) (observation, error) {
		status, err := read(ctx)
		if err != nil {
			return observation{}, err
		}
		if status.Revision != revision {
			return observation{}, connect.NewError(connect.CodeAborted, "the freeze changed while waiting; inspect "+inspect)
		}
		confirmed = status
		failed := status.Phase == cloudv1.FreezePhase_FREEZE_PHASE_UNSPECIFIED
		if freeze {
			failed = failed || status.Phase == cloudv1.FreezePhase_FREEZE_PHASE_UNFROZEN || status.Phase == cloudv1.FreezePhase_FREEZE_PHASE_UNFREEZING
		} else {
			failed = failed || status.Phase == cloudv1.FreezePhase_FREEZE_PHASE_FROZEN || status.Phase == cloudv1.FreezePhase_FREEZE_PHASE_FREEZING
		}
		return observation{state: convert.FreezePhaseText(status.Phase), message: status.Message, done: status.Phase == want, failed: failed}, nil
	})
	return confirmed, err
}

func AdminFreezeStatus(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := freezeTarget(o); err != nil {
		return nil, err
	}
	selected := freezeStatusRequest(o)
	response, err := call(ctx, selected, c.Admin.GetFreeze)
	if err != nil {
		return nil, err
	}
	return printFreezeStatus(o, convert.FreezeStatus(response.(*cloudv1.GetFreezeResponse), selected.Target))
}

func freezeTargetText(target *cloudv1.FreezeTarget) string {
	switch {
	case target.GetTenantId() != "":
		return "tenant " + target.GetTenantId()
	case target.GetNetworkId() != "":
		return "network " + target.GetNetworkId()
	case target.GetClusterId() != "":
		return "cluster " + target.GetClusterId()
	default:
		return "platform"
	}
}

func printFreezeStatus(o Options, status *cloudv1.FreezeStatus) (proto.Message, error) {
	if !isTable(o.Output) {
		return status, nil
	}
	if err := fields(o.Out, [][2]string{{"Target", freezeTargetText(status.Target)}, {"State", convert.FreezePhaseText(status.Phase)}, {"Platform Frozen", yesNo(status.GetPlatformFrozen())}, {"Freeze", freezeLine(status.Freeze)},
		{"Clusters", fmt.Sprint(status.Clusters)}, {"Networks", fmt.Sprint(status.Networks)},
		{"Providers Paused", fmt.Sprintf("%d/%d", status.PausedResources, status.TotalResources)},
		{"Newly Paused (Confirmation)", fmt.Sprint(status.NewlyPausedResources)}, {"Let Through", strings.Join(status.Exceptions, ", ")}, {"Message", status.Message}}); err != nil {
		return nil, err
	}
	if len(status.GetScopes()) > 0 {
		rows := make([][]string, 0, len(status.Scopes))
		for _, scope := range status.Scopes {
			target := "tenant " + scope.TenantId
			if scope.NetworkId != "" {
				target = "network " + scope.NetworkId
			}
			if scope.ClusterId != "" {
				target = "cluster " + scope.ClusterId
			}
			action := "unfreeze"
			if scope.Frozen {
				action = "freeze"
			}
			rows = append(rows, []string{target, action, untilText(scope.Until), orNone(scope.Reason)})
		}
		if err := table(o.Out, []string{"SCOPE (LAST MATCH WINS)", "ACTION", "UNTIL", "REASON"}, rows); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
