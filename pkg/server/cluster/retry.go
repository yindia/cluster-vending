package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// upgradeItem is the RetryItem name of the automatic upgrade.
const upgradeItem = "upgrade"

// retryComponent is one component of a cluster as RetryCluster weighs it.
type retryComponent struct {
	Name    string
	Kind    cloudv1.RetryItemKind
	Phase   cloudv1.ComponentPhase
	Chart   string
	Message string
}

// RetryCluster records a request to try again what failed on a cluster;
// the controller acts on it on its next pass (model.Retry).
func (s *ClusterService) RetryCluster(ctx context.Context, req *cloudv1.RetryClusterRequest) (*cloudv1.RetryClusterResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	comps, err := s.retryComponents(ctx, row)
	if err != nil {
		return nil, err
	}
	rollout := &cloudv1.ClusterRollout{}
	if len(row.Rollout) == 0 || reconcilecluster.Lenient.Unmarshal(row.Rollout, rollout) != nil {
		rollout = nil
	}
	items, retry, err := retryPlan(comps, rollout, req.GetComponents(), req.GetUpgrade())
	if err != nil {
		return nil, err
	}
	if len(retry.Components) > 0 || retry.Upgrade {
		retry.At, retry.By = time.Now().UTC(), caller(ctx)
		if err := s.deps.Store.Clusters().RequestRetry(ctx, row.TenantID, row.ID, retry); err != nil {
			return nil, service.ToConnect(err)
		}
		s.logger().InfoContext(ctx, "cluster retry requested", "tenant_id", row.TenantID, "cluster_id", row.ID,
			"components", retry.Components, "upgrade", retry.Upgrade, "by", retry.By)
	}
	return &cloudv1.RetryClusterResponse{Items: items}, nil
}

// retryComponents are the cluster's platform components, as its status
// records them, and its tenant components (but those being removed).
func (s *ClusterService) retryComponents(ctx context.Context, row *model.Cluster) ([]retryComponent, error) {
	st := &cloudv1.ClusterStatus{}
	_ = reconcilecluster.Lenient.Unmarshal(row.Status, st) // unreadable: no platform components to weigh
	var out []retryComponent
	for _, p := range st.GetPlatformComponents() {
		out = append(out, retryComponent{Name: p.GetName(), Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_PLATFORM_COMPONENT,
			Phase: p.GetPhase(), Chart: p.GetChart(), Message: p.GetMessage()})
	}
	rows, err := s.deps.Store.Components().ListByCluster(ctx, row.TenantID, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	for _, c := range rows {
		if c.State == cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING.String() {
			continue
		}
		cs := &cloudv1.ComponentStatus{}
		_ = reconcilecluster.Lenient.Unmarshal(c.Status, cs) // unreadable: weighed as not failed
		out = append(out, retryComponent{Name: c.Component, Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_TENANT_COMPONENT,
			Phase: cs.GetPhase(), Chart: cs.GetChart(), Message: cs.GetMessage()})
	}
	return out, nil
}

// retryPlan decides what a RetryCluster does: the named components (or,
// with none named and upgrade false, every failed one and a failed
// upgrade), and the upgrade with upgrade true. A failed component whose
// chart failed is tried again; one that failed before it had a chart (a
// precondition: IRSA off, a requirement missing, not in the release) has
// something to fix first, which its message says. The request records
// only what is tried again.
func retryPlan(comps []retryComponent, rollout *cloudv1.ClusterRollout, names []string, upgrade bool) ([]*cloudv1.RetryItem, model.Retry, error) {
	everything := len(names) == 0 && !upgrade
	byName := map[string]retryComponent{}
	for _, c := range comps {
		byName[c.Name] = c
	}
	var picked []retryComponent
	for _, n := range names {
		c, ok := byName[n]
		if !ok {
			return nil, model.Retry{}, connect.NewError(connect.CodeNotFound, "the cluster has no component "+n)
		}
		picked = append(picked, c)
	}
	if everything {
		for _, c := range comps {
			if c.Phase == cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED {
				picked = append(picked, c)
			}
		}
	}
	var items []*cloudv1.RetryItem
	var retry model.Retry
	for _, c := range picked {
		item := &cloudv1.RetryItem{Name: c.Name, Kind: c.Kind}
		switch {
		case c.Phase != cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED:
			item.Action, item.Message = cloudv1.RetryAction_RETRY_ACTION_NOTHING, fmt.Sprintf("%s is %s: nothing to retry", c.Name, phaseWord(c.Phase))
		case c.Chart == "":
			item.Action = cloudv1.RetryAction_RETRY_ACTION_FIX
			item.Message = "change this first; it installs by itself once done: " + c.Message
		default:
			item.Action = cloudv1.RetryAction_RETRY_ACTION_RETRY
			item.Message = fmt.Sprintf("chart %s is tried again on the next pass (its failures and recoveries start over)", c.Chart)
			retry.Components = append(retry.Components, c.Name)
		}
		items = append(items, item)
	}
	if everything || upgrade {
		if item, ok := upgradeRetry(rollout, upgrade); ok {
			items = append(items, item)
			retry.Upgrade = item.GetAction() == cloudv1.RetryAction_RETRY_ACTION_RETRY
		}
	}
	return items, retry, nil
}

// upgradeRetry is the upgrade's item: RETRY for a failed or timed-out
// automatic upgrade; NOTHING when asked for one and there is none. ok is
// false when nothing is to be said (everything asked, no failed upgrade).
func upgradeRetry(rollout *cloudv1.ClusterRollout, asked bool) (*cloudv1.RetryItem, bool) {
	item := &cloudv1.RetryItem{Name: upgradeItem, Kind: cloudv1.RetryItemKind_RETRY_ITEM_KIND_UPGRADE}
	switch rollout.GetResult() {
	case cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_FAILED, cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_TIMED_OUT:
		item.Action = cloudv1.RetryAction_RETRY_ACTION_RETRY
		item.Message = fmt.Sprintf("the automatic upgrade to %s is in progress again, with a new upgrade-timeout; it no longer counts against channel %s's maxFailed",
			rollout.GetToVersion(), rollout.GetChannel())
		return item, true
	case cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_UNSPECIFIED,
		cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_IN_PROGRESS,
		cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_SUCCEEDED,
		cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_PENDING,
		cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_BLOCKED:
	}
	if !asked {
		return nil, false
	}
	item.Action, item.Message = cloudv1.RetryAction_RETRY_ACTION_NOTHING, "no automatic upgrade has failed: nothing to retry"
	return item, true
}

// phaseWord is a component phase as one word: "Ready", "Installing".
func phaseWord(p cloudv1.ComponentPhase) string {
	w := strings.ToLower(strings.TrimPrefix(p.String(), "COMPONENT_PHASE_"))
	if w == "" || w == "unspecified" {
		return "not installed"
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

// caller is who asks, for the record: the token's username or subject.
func caller(ctx context.Context) string {
	id := auth.FromContext(ctx)
	switch {
	case id == nil || id.Anonymous:
		return "anonymous"
	case id.Username != "":
		return id.Username
	case id.Subject != "":
		return id.Subject
	default:
		return "anonymous"
	}
}
