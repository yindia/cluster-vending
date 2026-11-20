package cluster

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// removalComponent is one platform component's recorded objects: its Helm
// Releases (the charts, uninstalled first) and its IAM objects (removed once
// the charts are gone).
type removalComponent struct {
	name     string
	sequence int32
	releases []model.ManagedResource
	iam      []model.ManagedResource
}

// platformRemoval is the platform components' objects in one pass of a
// deletion: the records, their views as the pass started, and the
// components they belong to, highest sequence first.
type platformRemoval struct {
	records    []model.ManagedResource
	views      map[string]*cloudv1.ManagedResource // by kind/name
	components []*removalComponent
	order      definitionOrder
}

func viewKey(kind, name string) string { return kind + "/" + name }

func (p *platformRemoval) gone(r model.ManagedResource) bool {
	v, ok := p.views[viewKey(r.Kind, r.Name)]
	return !ok || v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
}

func (p *platformRemoval) allGone(rs []model.ManagedResource) bool {
	return !slices.ContainsFunc(rs, func(r model.ManagedResource) bool { return !p.gone(r) })
}

// loadPlatformRemoval reads the platform components' records and groups
// the objects still there by component (the component label every object
// carries), in removal order. Usages are no component's: Kubernetes
// collects them with their users and the orphans are released with the
// stage.
func (s *Reconciler) loadPlatformRemoval(ctx context.Context, row *model.Cluster) (*platformRemoval, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerClusterComponents, row.ID)
	if err != nil {
		return nil, fmt.Errorf("list platform component resources: %w", err)
	}
	p := &platformRemoval{records: records, order: s.definitionOrder(ctx)}
	if err := p.observe(ctx, s, records); err != nil {
		return nil, err
	}
	byName := map[string]*removalComponent{}
	for _, r := range records {
		if r.Kind == managed.UsageKind || p.gone(r) {
			continue
		}
		name := ""
		if u, err := s.deps.ReadRecorded(ctx, r); err == nil && u != nil {
			name = u.GetLabels()[crossplane.LabelComponent]
		}
		c := byName[name]
		if c == nil {
			c = &removalComponent{name: name, sequence: p.order.sequenceOf(name)}
			byName[name] = c
			p.components = append(p.components, c)
		}
		if r.Kind == crossplaneaws.HelmReleaseGVK.Kind {
			c.releases = append(c.releases, r)
		} else {
			c.iam = append(c.iam, r)
		}
	}
	slices.SortFunc(p.components, func(a, b *removalComponent) int {
		if a.sequence != b.sequence {
			return int(b.sequence - a.sequence)
		}
		return strings.Compare(a.name, b.name)
	})
	return p, nil
}

func (p *platformRemoval) observe(ctx context.Context, s *Reconciler, records []model.ManagedResource) error {
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return err
	}
	p.views = make(map[string]*cloudv1.ManagedResource, len(views))
	for _, v := range views {
		p.views[viewKey(v.GetKind(), v.GetName())] = v
	}
	return nil
}

// pending are the components of a sequence range that still have objects.
func (p *platformRemoval) pending(above bool) []*removalComponent {
	var out []*removalComponent
	for _, c := range p.components {
		if (c.sequence > 0) == above {
			out = append(out, c)
		}
	}
	return out
}

// removeAbove is stage 3: the platform components above sequence 0, highest
// sequence first. One sequence at a time, and in it a component only once
// no remaining component requires it; a component's charts are uninstalled,
// and its IAM objects removed once they are gone. The next sequence starts
// only when this one is fully gone. It reports whether they are all gone.
func (s *Reconciler) removeAbove(ctx context.Context, row *model.Cluster, p *platformRemoval) (bool, error) {
	pending := p.pending(true)
	if len(pending) == 0 {
		return true, nil
	}
	top := pending[0].sequence
	var group []*removalComponent
	var lower []string
	for _, c := range pending {
		if c.sequence == top {
			group = append(group, c)
		} else if !slices.Contains(lower, fmt.Sprint(c.sequence)) {
			lower = append(lower, fmt.Sprint(c.sequence))
		}
	}
	waits, err := s.removeGroup(ctx, row, p, group)
	if err != nil {
		return false, err
	}
	if len(lower) > 0 {
		waits += "; next sequence " + lower[0]
	}
	setDeleteStage(ctx, stagePlatform, fmt.Sprintf("uninstalling sequence %d; %s", top, waits))
	return false, nil
}

// removeKarpenterChart is the platform part of stage 5: Karpenter's chart
// (sequence 0) and its IAM role. It reports whether they are gone.
func (s *Reconciler) removeKarpenterChart(ctx context.Context, row *model.Cluster, p *platformRemoval) (bool, error) {
	group := p.pending(false)
	if len(group) == 0 {
		return true, nil
	}
	waits, err := s.removeGroup(ctx, row, p, group)
	if err != nil {
		return false, err
	}
	setDeleteStage(ctx, stageKarpenter, "uninstalling its chart and removing its IAM; "+waits)
	return false, nil
}

// removeGroup is one step for the components of a sequence: each component
// no remaining one of the group requires has its charts uninstalled, then
// its IAM objects removed. It returns what the step waits for.
func (s *Reconciler) removeGroup(ctx context.Context, row *model.Cluster, p *platformRemoval, group []*removalComponent) (string, error) {
	names := make([]string, 0, len(group))
	for _, c := range group {
		names = append(names, c.name)
	}
	var (
		waits   []string
		started []model.ManagedResource
	)
	for _, c := range group {
		if p.order.requiredBy(c.name, names) {
			waits = append(waits, c.name+" waits for the components that require it")
			continue
		}
		if len(c.releases) > 0 && !p.allGone(c.releases) {
			why, err := s.prepareReleases(ctx, row, p, c)
			if err != nil {
				return "", err
			}
			waits = append(waits, why)
			started = append(started, c.releases...)
			continue
		}
		started = append(started, c.iam...)
		waits = append(waits, c.name+": removing its IAM")
	}
	if err := s.finishRemoval(ctx, p, started); err != nil {
		return "", err
	}
	return strings.Join(waits, "; "), nil
}

// finishRemoval deletes the records, then reads them again: what went
// is marked, Usages whose user went are released.
func (s *Reconciler) finishRemoval(ctx context.Context, p *platformRemoval, started []model.ManagedResource) error {
	if len(started) > 0 {
		if err := s.deps.DeleteRecorded(ctx, started); err != nil {
			return deleteError(err)
		}
	}
	views, err := s.deps.Observe(ctx, p.records)
	if err != nil {
		return err
	}
	if err := s.deps.MarkGone(ctx, p.records, views); err != nil {
		return err
	}
	return s.deps.ReleaseOrphanUsages(ctx, p.records, views)
}

// prepareReleases makes sure nothing of the platform's own Helm
// reliability machinery stands in the way of a component's uninstall, and
// says what the uninstall waits for: the backoff pause (and the give-up
// mark) the platform put on a Release is lifted; a Release paused by anyone
// else stays paused, and the message says so; a Release stuck in a Helm
// state an interrupted operation left (uninstalling, pending-*) is
// recovered as a stuck install is (recoverStuck). The one-operation gate
// only governs writes of a Release's spec and plays no part.
func (s *Reconciler) prepareReleases(ctx context.Context, row *model.Cluster, p *platformRemoval, c *removalComponent) (string, error) {
	at := now()
	var blocked, stuckMsg string
	waitingFor := ""
	for _, r := range c.releases {
		if p.gone(r) {
			continue
		}
		u, err := s.deps.ReadRecorded(ctx, r)
		if err != nil {
			return "", fmt.Errorf("read Release %s: %w", r.Name, err)
		}
		if u == nil {
			continue
		}
		d := detailOf(u)
		d.deleting = !u.GetDeletionTimestamp().IsZero()
		if waitingFor == "" {
			waitingFor = fmt.Sprintf("%s: waiting for Release %s to be uninstalled", c.name, r.Name)
			if d.HelmState != "" {
				waitingFor += " (Helm state " + d.HelmState + ")"
			}
		}
		if d.PausedByUs {
			s.setBackoff(ctx, row, r.Namespace, r.Name, time.Time{}, false)
			d.PausedByUs = false
		}
		if d.Paused && blocked == "" {
			blocked = fmt.Sprintf("%s: Release %s is paused (crossplane.io/paused) by an operator or a freeze, so it is not uninstalled; "+
				"unpause it to go on", c.name, r.Name)
		}
		if stuckMsg == "" {
			stuckMsg = s.recoverDeleting(ctx, row, r.Namespace, c.name, d, at)
		}
	}
	switch {
	case blocked != "":
		return blocked, nil
	case stuckMsg != "":
		return waitingFor + "; " + stuckMsg, nil
	}
	return waitingFor, nil
}

// recoverDeleting recovers a Release whose Helm state is stuck
// (recoverStuck, with the recoveries counted on the component and kept in
// the status); it returns what happened, or "".
func (s *Reconciler) recoverDeleting(ctx context.Context, row *model.Cluster, namespace, name string, d *releaseDetail, at time.Time) string {
	if !stuck(d, at) {
		return ""
	}
	c := recoveryComponent(ctx, row, name)
	s.recoverStuck(ctx, row, namespace, c, d, at)
	noteRecovery(ctx, c)
	return c.GetMessage()
}
