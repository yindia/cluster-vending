package reconcile

import (
	"context"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

type FreezeTarget struct{ TenantID, NetworkID, ClusterID string }

func (target FreezeTarget) Matches(rule cloudv1alpha1.FreezeRule) bool {
	switch {
	case rule.ClusterID != "":
		return target.ClusterID == rule.ClusterID
	case rule.NetworkID != "":
		return target.NetworkID == rule.NetworkID
	case rule.TenantID != "":
		return target.TenantID == rule.TenantID
	default:
		return true
	}
}

func RuleActive(rule cloudv1alpha1.FreezeRule, now time.Time) bool {
	return rule.Until == nil || rule.Until.After(now)
}

func GlobalFreezeActive(hold *cloudv1alpha1.ReleaseHold, now time.Time) bool {
	return hold != nil && hold.Spec.Platform && !hold.Spec.Scoped && (hold.Spec.Until == nil || hold.Spec.Until.After(now))
}

func AnyFreezeActive(hold *cloudv1alpha1.ReleaseHold, now time.Time) bool {
	if GlobalFreezeActive(hold, now) {
		return true
	}
	if hold == nil {
		return false
	}
	for _, rule := range hold.Spec.Rules {
		if rule.Frozen && RuleActive(rule, now) {
			return true
		}
	}
	return false
}

func FreezeForTarget(hold *cloudv1alpha1.ReleaseHold, now time.Time, target FreezeTarget) *model.Freeze {
	if hold == nil || !hold.Spec.Platform {
		return nil
	}
	covered, held := GlobalFreezeActive(hold, now), GlobalFreezeActive(hold, now)
	frozen := &model.Freeze{Scope: "platform", By: hold.Spec.By, Since: hold.CreationTimestamp.Time, Reason: hold.Spec.Reason}
	if hold.Spec.Until != nil {
		until := hold.Spec.Until.Time
		frozen.Until = &until
	}
	for _, except := range hold.Spec.Except {
		if except != "" && (except == target.ClusterID || except == target.NetworkID) {
			held = false
		}
	}
	for _, rule := range hold.Spec.Rules {
		if !RuleActive(rule, now) || !target.Matches(rule) {
			continue
		}
		held = rule.Frozen
		if rule.Frozen {
			covered = true
			switch {
			case rule.ClusterID != "":
				frozen.Scope = "cluster " + rule.ClusterID
			case rule.NetworkID != "":
				frozen.Scope = "network " + rule.NetworkID
			case rule.TenantID != "":
				frozen.Scope = "tenant " + rule.TenantID
			}
			frozen.By, frozen.Reason, frozen.Since, frozen.Until = rule.By, rule.Reason, rule.Since.Time, nil
			if rule.Until != nil {
				until := rule.Until.Time
				frozen.Until = &until
			}
		}
	}
	if !covered {
		return nil
	}
	frozen.LetThrough = !held
	return frozen
}

func (d Deps) ResolveFreezeTarget(ctx context.Context, target FreezeTarget) (FreezeTarget, error) {
	if target.ClusterID != "" && (target.NetworkID == "" || target.TenantID == "") {
		clusters := &cloudv1alpha1.ClusterList{}
		if err := d.Reader().List(ctx, clusters, client.MatchingLabels{"cloud.cluster-vending.io/id": target.ClusterID}); err != nil {
			return target, err
		}
		if len(clusters.Items) > 0 {
			cluster := &clusters.Items[0]
			target.TenantID = strings.TrimPrefix(cluster.Namespace, "tenant-")
			if cluster.Labels["cloud.cluster-vending.io/tenant"] != "" {
				target.TenantID = cluster.Labels["cloud.cluster-vending.io/tenant"]
			}
			if cluster.Spec.Network.NetworkRef.Name != "" {
				network := &cloudv1alpha1.Network{}
				if err := d.Reader().Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Spec.Network.NetworkRef.Name}, network); err != nil {
					return target, err
				}
				target.NetworkID = network.Labels["cloud.cluster-vending.io/id"]
			}
		}
	}
	if target.NetworkID != "" && target.TenantID == "" {
		networks := &cloudv1alpha1.NetworkList{}
		if err := d.Reader().List(ctx, networks, client.MatchingLabels{"cloud.cluster-vending.io/id": target.NetworkID}); err != nil {
			return target, err
		}
		if len(networks.Items) > 0 {
			target.TenantID = strings.TrimPrefix(networks.Items[0].Namespace, "tenant-")
		}
	}
	return target, nil
}

func (d Deps) FreezeFor(ctx context.Context, target FreezeTarget) (*model.Freeze, error) {
	if d.Kube == nil && d.KubeReader == nil {
		source, ok := d.Platform.(HoldReader)
		if !ok {
			return nil, nil
		}
		holds, err := source.Holds(ctx)
		if err != nil {
			return nil, err
		}
		for index := range holds {
			if frozen := FreezeForTarget(&holds[index], time.Now(), target); frozen != nil {
				return frozen, nil
			}
		}
		return nil, nil
	}
	hold := &cloudv1alpha1.ReleaseHold{}
	if err := d.Reader().Get(ctx, client.ObjectKey{Name: FreezeName}, hold); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		return nil, err
	}
	if GlobalFreezeActive(hold, time.Now()) && len(hold.Spec.Rules) == 0 && len(hold.Spec.Except) == 0 {
		return FreezeForTarget(hold, time.Now(), target), nil
	}
	target, err := d.ResolveFreezeTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	return FreezeForTarget(hold, time.Now(), target), nil
}
func EffectiveFreezeActive(hold *cloudv1alpha1.ReleaseHold, targets map[string]FreezeTarget, now time.Time) bool {
	if GlobalFreezeActive(hold, now) {
		return true
	}
	if hold == nil {
		return false
	}
	for _, target := range targets {
		if FreezeForTarget(hold, now, target).Holds() {
			return true
		}
	}
	for _, rule := range hold.Spec.Rules {
		if rule.TenantID != "" && FreezeForTarget(hold, now, FreezeTarget{TenantID: rule.TenantID}).Holds() {
			return true
		}
	}
	return false
}
