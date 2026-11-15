package admin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/rollout"
)

// pauseTarget is what a pause or a resume names: a tenant (with one of its
// clusters or networks), or a release channel (with a tier: "0", "1",
// "2"; "" for every tier).
type pauseTarget struct {
	tenant, cluster, network string
	channel, tier            string
}

func pauseTargetOf(tenant, cluster, network string, channel cloudv1.ReleaseChannel, tier cloudv1.ClusterTier) pauseTarget {
	t := pauseTarget{tenant: tenant, cluster: cluster, network: network}
	if channel != cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED {
		t.channel = strings.ToLower(strings.TrimPrefix(channel.String(), "RELEASE_CHANNEL_"))
	}
	if tier != cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED {
		t.tier = strings.TrimPrefix(tier.String(), "CLUSTER_TIER_")
	}
	return t
}

// rows is the target as the store pauses rows.
func (t pauseTarget) rows() model.PauseTarget {
	return model.PauseTarget{ClusterID: t.cluster, NetworkID: t.network}
}

// holdSpec is the target as a release hold (a freeze) names it.
func (t pauseTarget) holdSpec() cloudv1alpha1.ReleaseHoldSpec {
	switch {
	case t.cluster != "":
		return cloudv1alpha1.ReleaseHoldSpec{ClusterID: t.cluster}
	case t.channel != "":
		spec := cloudv1alpha1.ReleaseHoldSpec{Channel: t.channel}
		if t.tier != "" {
			spec.Tier = cloudv1alpha1.ClusterTier("Tier" + t.tier)
		}
		return spec
	}
	return cloudv1alpha1.ReleaseHoldSpec{TenantID: t.tenant}
}

// clusters reads every cluster of every tenant.
func (s *Service) clusters(ctx context.Context) ([]cloudv1alpha1.Cluster, error) {
	list := &cloudv1alpha1.ClusterList{}
	if err := s.deps.Reader().List(ctx, list); err != nil {
		s.logger().ErrorContext(ctx, "list clusters", "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "cannot list the clusters; try again")
	}
	return list.Items, nil
}

// matching are the clusters a hold's target covers, those being deleted
// left out.
func matching(clusters []cloudv1alpha1.Cluster, spec cloudv1alpha1.ReleaseHoldSpec) []cloudv1alpha1.Cluster {
	var out []cloudv1alpha1.Cluster
	for i := range clusters {
		if clusters[i].DeletionTimestamp.IsZero() && rollout.Matches(spec, &clusters[i]) {
			out = append(out, clusters[i])
		}
	}
	return out
}

// clustersOf counts the clusters a target covers now; 0 when they cannot be
// read (the count is for people, the pause is done).
func (s *Service) clustersOf(ctx context.Context, t pauseTarget) int32 {
	if t.network != "" {
		return 0
	}
	all, err := s.clusters(ctx)
	if err != nil {
		return 0
	}
	return count(matching(all, t.holdSpec()))
}

func count[T any](items []T) int32 { return int32(min(len(items), math.MaxInt32)) }

// pauseChannel pauses every cluster on the channel (of the tier) now, each
// at the channel's scope (model.ChannelScope): a resume of the channel
// removes that scope only. A cluster that joins the channel later is not
// paused.
func (s *Service) pauseChannel(ctx context.Context, t pauseTarget, pause *model.Pause) (*cloudv1.PauseReconcileResponse, error) {
	all, err := s.clusters(ctx)
	if err != nil {
		return nil, err
	}
	scope := model.ChannelScope(t.channel, t.tier)
	matched := matching(all, t.holdSpec())
	out := &cloudv1.PauseReconcileResponse{Clusters: count(matched), Note: fmt.Sprintf(
		"paused the %d clusters on %s now; a cluster that joins it later is not paused (pause it with --cluster, or freeze the channel to stop automatic changes of what joins it)",
		len(matched), scope.Text())}
	for _, c := range matched {
		tenant, id := tenantOf(&c), c.Labels[cloudstore.LabelID]
		target := model.PauseTarget{ClusterID: id, Selector: scope}
		if err := s.deps.Store.Tenants().SetPause(ctx, tenant, target, pause); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue // deleted meanwhile
			}
			return out, s.storeError(ctx, "pause", tenant, target, err)
		}
		n, err := s.setPaused(ctx, tenant, model.PauseTarget{ClusterID: id}, true)
		out.Resources += n
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// resumeChannel removes the channel's (and tier's) pause from each cluster
// that has it, and resumes the providers on the clusters no other pause
// holds: a cluster also paused on its own stays paused.
func (s *Service) resumeChannel(ctx context.Context, t pauseTarget) (*cloudv1.ResumeReconcileResponse, error) {
	all, err := s.clusters(ctx)
	if err != nil {
		return nil, err
	}
	scope := model.ChannelScope(t.channel, t.tier)
	out := &cloudv1.ResumeReconcileResponse{}
	for i := range all {
		c := &all[i]
		if _, ok := model.ParsePause(c.Annotations[cloudstore.AnnotationPaused]).Entry(scope); !ok {
			continue
		}
		tenant, id := tenantOf(c), c.Labels[cloudstore.LabelID]
		target := model.PauseTarget{ClusterID: id, Selector: scope}
		if err := s.deps.Store.Tenants().SetPause(ctx, tenant, target, nil); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			return out, s.storeError(ctx, "resume", tenant, target, err)
		}
		out.Clusters++
		row, err := s.deps.Store.Clusters().Get(ctx, tenant, id)
		if err != nil || row.Paused != nil {
			continue // gone, or still paused by something else
		}
		n, err := s.setPaused(ctx, tenant, model.PauseTarget{ClusterID: id}, false)
		out.Resources += n
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// tenantOf is a cluster's tenant: its label, else its namespace's name.
func tenantOf(c *cloudv1alpha1.Cluster) string {
	if t := c.Labels[cloudstore.LabelTenant]; t != "" {
		return t
	}
	return strings.TrimPrefix(c.Namespace, "tenant-")
}

// ListPauses lists every pause in effect, once per target: a tenant's (on
// its namespace), a network's or a cluster's own, a channel's (and
// tier's) with the clusters it paused, then the freezes. A scope whose
// until has passed is left out.
func (s *Service) ListPauses(ctx context.Context, _ *cloudv1.ListPausesRequest) (*cloudv1.ListPausesResponse, error) {
	all, err := s.clusters(ctx)
	if err != nil {
		return nil, err
	}
	l := pauseList{now: s.now(), byTarget: map[string]*cloudv1.PauseRecord{}}
	namespaces := &corev1.NamespaceList{}
	networks := &cloudv1alpha1.NetworkList{}
	holds := &cloudv1alpha1.ReleaseHoldList{}
	for _, list := range []client.ObjectList{namespaces, networks, holds} {
		if err := s.deps.Reader().List(ctx, list); err != nil {
			s.logger().ErrorContext(ctx, "list the pauses", "error", err)
			return nil, connect.NewError(connect.CodeUnavailable, "cannot read the pauses; try again")
		}
	}
	tenantClusters := map[string]int32{}
	for i := range all {
		tenantClusters[tenantOf(&all[i])]++
	}
	for _, ns := range namespaces.Items {
		tenant := cmp.Or(ns.Labels[cloudstore.LabelTenant], strings.TrimPrefix(ns.Name, "tenant-"))
		l.add(ns.Annotations, model.PauseTenant, &cloudv1.PauseRecord{Target: "tenant " + tenant, TenantId: tenant,
			Clusters: tenantClusters[tenant]})
	}
	for _, n := range networks.Items {
		id := n.Labels[cloudstore.LabelID]
		tenant := cmp.Or(n.Labels[cloudstore.LabelTenant], strings.TrimPrefix(n.Namespace, "tenant-"))
		l.add(n.Annotations, model.PauseNetwork, &cloudv1.PauseRecord{Target: "network " + id, TenantId: tenant, NetworkId: id})
	}
	for i := range all {
		c := &all[i]
		id := c.Labels[cloudstore.LabelID]
		l.add(c.Annotations, model.PauseCluster, &cloudv1.PauseRecord{Target: "cluster " + id, TenantId: tenantOf(c), ClusterId: id, Clusters: 1})
	}
	out := &cloudv1.ListPausesResponse{}
	for _, r := range l.byTarget {
		out.Pauses = append(out.Pauses, r)
	}
	for i := range holds.Items {
		h := &holds.Items[i]
		if !rollout.Active(*h, l.now) {
			continue
		}
		out.Pauses = append(out.Pauses, freezeRecord(h, count(matching(all, h.Spec))))
	}
	slices.SortFunc(out.Pauses, func(a, b *cloudv1.PauseRecord) int {
		return cmp.Or(cmp.Compare(a.GetKind(), b.GetKind()), cmp.Compare(a.GetTarget(), b.GetTarget()))
	})
	return out, nil
}

// pauseList gathers the full pauses, one record per target.
type pauseList struct {
	now      time.Time
	byTarget map[string]*cloudv1.PauseRecord
}

// add records the scopes of the pause an object's annotations carry: own
// (the object's own scope: its tenant, network or cluster; a pause of an
// older build without scopes counts as its own) as record, a channel's
// counted once per cluster it paused. A tenant's pause is listed from its
// namespace only.
func (l pauseList) add(annotations map[string]string, own model.PauseScope, record *cloudv1.PauseRecord) {
	stored := model.ParsePause(annotations[cloudstore.AnnotationPaused])
	if stored == nil {
		return
	}
	p := stored.Active(l.now)
	if p == nil {
		return
	}
	entries := p.Entries
	if len(entries) == 0 {
		entries = []model.PauseEntry{{Scope: own, By: p.By, Since: p.Since, Reason: p.Reason, Until: p.Until}}
	}
	for _, e := range entries {
		var r *cloudv1.PauseRecord
		switch channel, tier, ok := e.Scope.Channel(); {
		case e.Scope == own:
			r = record
		case ok:
			key := e.Scope.Text()
			if existing := l.byTarget[key]; existing != nil {
				existing.Clusters++
				continue
			}
			r = &cloudv1.PauseRecord{Target: key, Channel: channelEnum(channel), Tier: tierEnum(tier), Clusters: 1}
		default:
			continue // a tenant's pause on its rows: listed from the namespace
		}
		r.Kind, r.By, r.Since, r.Reason = cloudv1.PauseKind_PAUSE_KIND_EVERYTHING, e.By, timestamppb.New(e.Since), e.Reason
		if e.Until != nil {
			r.Until = timestamppb.New(*e.Until)
		}
		l.byTarget[r.Target] = r
	}
}

func channelEnum(channel string) cloudv1.ReleaseChannel {
	if channel == "" {
		return cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED
	}
	return cloudv1.ReleaseChannel(cloudv1.ReleaseChannel_value["RELEASE_CHANNEL_"+strings.ToUpper(channel)])
}

func tierEnum(tier string) cloudv1.ClusterTier {
	if _, err := strconv.Atoi(tier); err != nil {
		return cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED
	}
	return cloudv1.ClusterTier(cloudv1.ClusterTier_value["CLUSTER_TIER_"+tier])
}
