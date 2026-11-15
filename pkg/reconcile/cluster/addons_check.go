package cluster

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Add-on versions are never mapped in the platform's release: a cluster's
// add-ons run the versions EKS recommends for its Kubernetes minor, read
// from AWS when the cluster is created or its version changes (resolve),
// and read again every addonsCheckEvery: when EKS recommends a newer
// version, the add-ons not pinned by the tenant move to it.
const (
	addonsCheckEvery = 24 * time.Hour
	// addonsCheckRetry: a check AWS did not answer is tried again after.
	addonsCheckRetry = time.Hour
)

// addonChecks are when each cluster's add-ons were last checked, kept in
// memory: a restarted controller checks each cluster once more.
type addonChecks struct {
	mu   sync.Mutex
	next map[string]time.Time
}

// due reports whether the cluster's add-ons are to be checked at at.
func (c *addonChecks) due(id string, at time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next, ok := c.next[id]
	return !ok || !at.Before(next)
}

func (c *addonChecks) checkAgain(id string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next == nil {
		c.next = map[string]time.Time{}
	}
	c.next[id] = at
}

// addonChange is an add-on moving to the version EKS recommends now.
type addonChange struct {
	Name, From, To string
}

// addonsCheckable reports whether the cluster's add-ons may be moved now:
// Running, applied, not paused, and no Kubernetes upgrade or release move
// under way (they set the versions themselves, in their own order).
func addonsCheckable(row *model.Cluster) bool {
	return row.State == StateRunning && row.Paused == nil && row.AppliedGeneration >= row.Generation &&
		!Upgrading(row) && !ReleaseMoving(row)
}

// addonChanges are the add-ons of spec whose recommended version differs
// from the one they run; pinned add-ons keep the tenant's version.
func addonChanges(spec *cloudv1.ClusterSpec, pinned, recommended map[string]string) []addonChange {
	var out []addonChange
	for _, a := range spec.GetAws().GetAddons() {
		want := recommended[a.GetName()]
		if _, ok := pinned[a.GetName()]; ok || want == "" || want == a.GetVersion() {
			continue
		}
		out = append(out, addonChange{Name: a.GetName(), From: a.GetVersion(), To: want})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// withAddonVersions is row with the changed add-on versions in its stored
// spec, a new generation the next apply brings to EKS (resolved already:
// the versions are AWS's).
func withAddonVersions(row *model.Cluster, changes []addonChange) error {
	spec, err := SpecOf(row)
	if err != nil {
		return err
	}
	to := map[string]string{}
	for _, c := range changes {
		to[c.Name] = c.To
	}
	for _, a := range spec.GetAws().GetAddons() {
		if v, ok := to[a.GetName()]; ok {
			a.Version = v
		}
	}
	if row.Spec, err = Encode(spec); err != nil {
		return err
	}
	row.Generation++
	row.ResolvedGeneration = row.Generation
	row.State = StateUpdating
	return nil
}

// recheckAddons is the daily check of a Running cluster's add-ons: it asks
// AWS for the versions EKS recommends for the cluster's minor and stores
// the changed ones (unpinned add-ons only), with an AddonUpgraded event
// each; the pass applies them. A check AWS does not answer is logged and
// tried again in an hour; it never fails the pass.
func (s *Reconciler) recheckAddons(ctx context.Context, row *model.Cluster) {
	at := now()
	if s.deps.AWSClusters == nil || !addonsCheckable(row) || !s.checks.due(row.ID, at) {
		return
	}
	// A frozen platform starts nothing on its own; checked again later.
	if frozen, why := s.frozen(ctx, row.ID); frozen {
		s.logger().DebugContext(ctx, "add-on versions not checked", "cluster_id", row.ID, "why", why)
		return
	}
	changes, err := s.recommendedChanges(ctx, row)
	if err != nil {
		s.checks.checkAgain(row.ID, at.Add(addonsCheckRetry))
		s.logger().WarnContext(ctx, "check the add-on versions EKS recommends", "cluster_id", row.ID, "error", err)
		return
	}
	if len(changes) == 0 {
		s.checks.checkAgain(row.ID, at.Add(addonsCheckEvery))
		return
	}
	before := *row
	if err := withAddonVersions(row, changes); err != nil {
		*row = before
		s.logger().WarnContext(ctx, "record new add-on versions", "cluster_id", row.ID, "error", err)
		return
	}
	if err := s.deps.Store.Clusters().Record(ctx, row); err != nil {
		*row = before // changed meanwhile, or not stored: checked again next pass
		s.logger().InfoContext(ctx, "new add-on versions not recorded; checked again next pass", "cluster_id", row.ID, "error", err)
		return
	}
	s.checks.checkAgain(row.ID, at.Add(addonsCheckEvery))
	for _, c := range changes {
		reconcile.AddNote(ctx, reconcile.Normal(reconcile.ReasonAddonUpgraded,
			fmt.Sprintf("upgrading add-on %s from %s to %s, the version EKS now recommends", c.Name, c.From, c.To)))
	}
}

// recommendedChanges asks AWS for the add-on versions EKS recommends for
// the cluster's Kubernetes version now.
func (s *Reconciler) recommendedChanges(ctx context.Context, row *model.Cluster) ([]addonChange, error) {
	spec, err := SpecOf(row)
	if err != nil {
		return nil, err
	}
	t, err := s.Target(ctx, row.TenantID, spec)
	if err != nil {
		return nil, err
	}
	pinned, err := pinnedOf(row)
	if err != nil {
		return nil, err
	}
	asked := proto.CloneOf(spec)
	// Every add-on unpinned: the answer is what EKS recommends (pinned ones
	// are left out of the changes).
	if err := s.resolveVersions(ctx, t.Account, asked, nil); err != nil {
		return nil, err
	}
	recommended := map[string]string{}
	for _, a := range asked.GetAws().GetAddons() {
		recommended[a.GetName()] = a.GetVersion()
	}
	return addonChanges(spec, pinned, recommended), nil
}
