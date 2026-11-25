package aws

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// PlatformPoolInput is nodeGroups.platform as the render takes it
// (docs/proposals/PROP-03-component-placement.md, 4.3 and 4.4). The platform
// pool is on-demand only: spot is refused.
type PlatformPoolInput struct {
	Families, Sizes []string
	// Architecture is "AMD64" or "ARM64".
	Architecture  string
	CapacityTypes []string
	// LimitCPU is in cores, LimitMemory a quantity such as "512Gi".
	LimitCPU    int64
	LimitMemory string
	// ConsolidationPolicy is WhenEmpty or WhenEmptyOrUnderutilized.
	ConsolidationPolicy                                   string
	ConsolidateAfter, ExpireAfter, TerminationGracePeriod time.Duration
	// DisruptionBudget is the nodes disrupted at once; MaxPods is the
	// kubelet's maxPods, 0 for Karpenter's default.
	DisruptionBudget, MaxPods int
}

func (p PlatformPoolInput) validate() error {
	var errs []error
	if len(p.Families) == 0 || len(p.Sizes) == 0 {
		errs = append(errs, errors.New("the platform pool needs instance families and sizes"))
	}
	if _, ok := archNamesByConfig[p.Architecture]; !ok {
		errs = append(errs, fmt.Errorf("the platform pool's architecture %q is not AMD64 or ARM64", p.Architecture))
	}
	if len(p.CapacityTypes) == 0 || slices.ContainsFunc(p.CapacityTypes, func(c string) bool { return c != "ON_DEMAND" }) {
		errs = append(errs, errors.New("the platform pool is on-demand only"))
	}
	if q, err := resource.ParseQuantity(p.LimitMemory); err != nil || q.Sign() <= 0 || p.LimitCPU <= 0 {
		errs = append(errs, errors.New("the platform pool's limits must be positive"))
	}
	if p.ConsolidationPolicy != "WhenEmpty" && p.ConsolidationPolicy != "WhenEmptyOrUnderutilized" {
		errs = append(errs, fmt.Errorf("the platform pool's consolidation policy %q is not WhenEmpty or WhenEmptyOrUnderutilized", p.ConsolidationPolicy))
	}
	if p.DisruptionBudget < 1 || p.ExpireAfter <= 0 || p.TerminationGracePeriod <= 0 || p.ConsolidateAfter < 0 || p.MaxPods < 0 {
		errs = append(errs, errors.New("the platform pool's disruption budget, expiry, grace period, consolidation delay and max pods are out of range"))
	}
	return errors.Join(errs...)
}

var archNamesByConfig = map[string]string{"AMD64": "amd64", "ARM64": "arm64"}

// renderPlatformPool is the EC2NodeClass and the NodePool PoolPlatform, a
// class before its pool. The pool's label and taint carry its name, so the
// platform's components select it with PoolLabels(PoolPlatform) and
// tolerate PoolTaint(PoolPlatform).
func renderPlatformPool(in KarpenterNodeGroupsInput) ([]*unstructured.Unstructured, error) {
	p := in.PlatformPool
	if err := p.validate(); err != nil {
		return nil, err
	}
	taint := PoolTaint(PoolPlatform)
	reqs := []any{
		inRequirement("kubernetes.io/arch", archNamesByConfig[p.Architecture]),
		inRequirement("karpenter.sh/capacity-type", "on-demand"),
		inRequirement("karpenter.k8s.aws/instance-family", sortedUnique(p.Families)...),
		inRequirement("karpenter.k8s.aws/instance-size", sortedUnique(p.Sizes)...),
	}
	spec := map[string]any{
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{PoolLabelKey: PoolPlatform}},
			"spec": map[string]any{
				"nodeClassRef":           map[string]any{"group": karpenterAPIGroup, "kind": KarpenterNodeClassGVK.Kind, "name": PoolPlatform},
				"taints":                 []any{map[string]any{"key": taint.Key, "value": taint.Value, "effect": "NoSchedule"}},
				"requirements":           reqs,
				"expireAfter":            durationString(p.ExpireAfter),
				"terminationGracePeriod": durationString(p.TerminationGracePeriod),
			},
		},
		"limits": map[string]any{"cpu": fmt.Sprintf("%d", p.LimitCPU), "memory": strings.TrimSpace(p.LimitMemory)},
		"disruption": map[string]any{
			"consolidationPolicy": p.ConsolidationPolicy,
			"consolidateAfter":    durationString(p.ConsolidateAfter),
			"budgets":             []any{map[string]any{"nodes": fmt.Sprintf("%d", p.DisruptionBudget)}},
		},
	}
	pool := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	pool.SetGroupVersionKind(KarpenterNodePoolGVK)
	pool.SetName(PoolPlatform)
	pool.SetLabels(map[string]string{LabelManagedBy: ManagedByPlatform})
	return []*unstructured.Unstructured{renderNodeClassWith(in, PoolPlatform, p.MaxPods), pool}, nil
}

// durationString is a duration as Karpenter reads it (5m, 336h, 1m30s).
func durationString(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}
