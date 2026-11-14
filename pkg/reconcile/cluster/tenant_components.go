package cluster

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// componentDeleting is the stored state DisableComponent gives a
// component: it is removed from its cluster, then from the store.
const componentDeleting = "COMPONENT_PHASE_DELETING"

// Messages of tenant components that are not installed for a reason of
// the cluster's.
const (
	tenantImportedMessage = "the cluster is imported read-only: components are installed once it is taken over"
	tenantKeptOffMessage  = "components are kept off for this cluster (it was taken over with skip_platform_components)"
	tenantNotRunning      = "waiting for the cluster to be RUNNING"
)

// installTenant installs the components the cluster's tenant enabled, the
// way installPlatform installs the platform's: each planned like a
// Platform component (component.PlanTenant: version and locked values from
// the cluster's release, the tenant's values under the platform's) and
// applied in order (crossplaneaws.PlatformSet), its objects recorded with
// the Component (model.OwnerComponent) so disabling it removes only its
// own. A component waits for every component it requires, the platform's
// included. Disabled components are removed (removeTenant). Each
// component's status is written back to it.
//
// Nothing is installed while the cluster is imported read-only, taken over
// with the platform components kept off, or not RUNNING yet; a paused
// cluster never gets here. platform is what this pass found out about the
// platform components (nil: as recorded).
func (s *Reconciler) installTenant(ctx context.Context, row *model.Cluster, p *platform) error {
	if s.deps.Platform == nil || row.State == StateDeleting {
		return nil
	}
	rows, err := s.deps.Store.Components().ListByCluster(ctx, row.TenantID, row.ID)
	if err != nil || len(rows) == 0 {
		return err
	}
	var errs []error
	var live []model.Component
	for _, c := range rows {
		if c.State == componentDeleting {
			if _, err := s.removeTenant(ctx, row, c); err != nil {
				errs = append(errs, fmt.Errorf("remove component %s: %w", c.Component, err))
			}
			continue
		}
		live = append(live, c)
	}
	if len(live) == 0 {
		return errors.Join(errs...)
	}
	if hold := tenantHold(row); hold != "" {
		for _, c := range live {
			errs = append(errs, s.setTenantStatus(ctx, row, c, &cloudv1.PlatformComponent{Name: c.Component,
				Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING, Message: hold}, ""))
		}
		return errors.Join(errs...)
	}
	in, release, problem, err := s.platformInput(ctx, row, storedStatus(row).GetPlatformRelease() != "")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	defs, err := s.deps.Platform.Definitions(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("read the component catalog: %w", err))...)
	}
	entries := component.TenantEntries(defs)
	if p == nil {
		p = storedPlatform(row)
	}
	ready := s.requirable(in, p, entries, live)
	for _, c := range live {
		errs = append(errs, s.installOneTenant(ctx, row, c, in, release, problem, entries, ready))
	}
	return errors.Join(errs...)
}

// tenantHold is why no tenant component is installed on the cluster now;
// "" when they may be.
func tenantHold(row *model.Cluster) string {
	switch {
	case row.Imported && (row.State == StateImported || row.State == StateBlocked):
		return tenantImportedMessage
	case noPlatform(row):
		return tenantKeptOffMessage
	case row.State != StateRunning && row.State != StateUpdating:
		return tenantNotRunning
	}
	return ""
}

// requirable names what a tenant component may wait for: every platform
// component that is not FAILED and every enabled tenant component, each
// with the last Release object of its install.
func (s *Reconciler) requirable(in component.PlatformInput, p *platform, entries []component.PlatformEntry,
	live []model.Component) map[string]string {
	out := map[string]string{}
	object := func(e component.PlatformEntry) string {
		return crossplaneaws.HelmReleaseObjectName(in.Plan.ClusterPrefix, component.LastRelease(e, in.Pins))
	}
	failed := map[string]bool{}
	for _, c := range p.components {
		if c.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED {
			failed[c.GetName()] = true
		}
	}
	for _, e := range in.Entries {
		if !failed[e.Name] {
			out[e.Name] = object(e)
		}
	}
	enabled := map[string]bool{}
	for _, c := range live {
		enabled[c.Component] = true
	}
	for _, e := range entries {
		if enabled[e.Name] {
			out[e.Name] = object(e)
		}
	}
	return out
}

// installOneTenant plans, applies and reports one tenant component.
func (s *Reconciler) installOneTenant(ctx context.Context, row *model.Cluster, c model.Component, in component.PlatformInput,
	release, problem string, entries []component.PlatformEntry, ready map[string]string) error {
	pc := &cloudv1.PlatformComponent{Name: c.Component, Version: in.Pins[c.Component].Version}
	fail := func(why string) error {
		pc.Phase, pc.Message = cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, why
		return s.setTenantStatus(ctx, row, c, pc, release)
	}
	if problem != "" {
		return fail(problem)
	}
	var entry *component.PlatformEntry
	for i := range entries {
		if entries[i].Name == c.Component {
			entry = &entries[i]
		}
	}
	if entry == nil {
		return fail(fmt.Sprintf("component %s is not in the catalog (missing, not valid, or disabled)", c.Component))
	}
	tenant, err := component.ParseValues(c.Values)
	if err != nil {
		return fail("its stored values: " + err.Error())
	}
	self := map[string]string{}
	for name, object := range ready {
		if name != c.Component {
			self[name] = object
		}
	}
	plan, reason := component.PlanTenant(in, *entry, tenant, self)
	if reason != "" {
		return fail(reason)
	}
	owner := reconcile.Owner{TenantID: row.TenantID, CloudAccountID: row.CloudAccountID, Kind: model.OwnerComponent, ID: c.ID}
	res, err := s.deps.Sequence().Apply(ctx, crossplaneaws.PlatformSet(plan), s.deps.Inventory(owner))
	if err != nil {
		s.logger().ErrorContext(ctx, "apply tenant component", "cluster_id", row.ID, "component_id", c.ID, "error", err)
		return fmt.Errorf("apply component %s: %w", c.Component, err)
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerComponent, c.ID)
	if err != nil {
		return err
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return err
	}
	one := component.PlatformInput{Entries: []component.PlatformEntry{*entry}, Pins: in.Pins, Release: in.Release}
	components, objects := platformStatus(one, plan, nil, res, views)
	if len(components) == 1 {
		pc = components[0]
	}
	prev := tenantStatus(c)
	if prev.GetTargetVersion() == pc.GetVersion() {
		pc.Recoveries = prev.GetRecoveries()
	}
	if object := objects[c.Component]; object != "" {
		if d := s.releaseDetail(ctx, plan.Namespace, object); d != nil {
			pc.Chart, pc.HelmState, pc.LastError, pc.Failures = d.Chart, d.HelmState, d.LastError, d.Failures
			s.recoverStuck(ctx, row, plan.Namespace, pc, d, now())
		}
	}
	return s.setTenantStatus(ctx, row, c, pc, release)
}

// tenantStatus is the status recorded with a component; empty when there
// is none this build can read.
func tenantStatus(c model.Component) *cloudv1.ComponentStatus {
	st := &cloudv1.ComponentStatus{}
	if len(c.Status) > 0 && Lenient.Unmarshal(c.Status, st) != nil {
		return &cloudv1.ComponentStatus{}
	}
	return st
}

// setTenantStatus writes what a pass found out about a component to its
// status, keeping what it does not say (the effective values the API
// recorded), and logs a phase change once. A component that changed
// since it was read is left: the next pass writes it.
func (s *Reconciler) setTenantStatus(ctx context.Context, row *model.Cluster, c model.Component, pc *cloudv1.PlatformComponent, release string) error {
	prev := tenantStatus(c)
	st := &cloudv1.ComponentStatus{EffectiveValues: prev.GetEffectiveValues(), ClusterName: row.Name,
		Phase: pc.GetPhase(), Message: pc.GetMessage(), TargetVersion: pc.GetVersion(), Release: release,
		InstalledVersion: prev.GetInstalledVersion(), ObservedGeneration: c.Generation,
		Chart: pc.GetChart(), HelmState: pc.GetHelmState(), LastError: pc.GetLastError(),
		Failures: pc.GetFailures(), Recoveries: pc.GetRecoveries()}
	if st.GetTargetVersion() == "" {
		st.TargetVersion = prev.GetTargetVersion()
	}
	if st.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
		st.InstalledVersion = st.GetTargetVersion()
	}
	if prev.GetPhase() == st.GetPhase() && prev.GetSince() != nil {
		st.Since = prev.GetSince()
	} else {
		st.Since = timestamppb.New(now())
		s.logTenant(ctx, row, c, st)
	}
	raw, err := protojson.Marshal(st)
	if err != nil {
		return err
	}
	if err := s.deps.Store.Components().SetStatus(ctx, c.TenantID, c.ID, c.ETag, raw); err != nil &&
		!errors.Is(err, repository.ErrConflict) && !errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("record the status of component %s: %w", c.Component, err)
	}
	return nil
}

// logTenant logs a tenant component's new phase, once: warn when it
// failed, info when it installs, is ready or is being removed.
func (s *Reconciler) logTenant(ctx context.Context, row *model.Cluster, c model.Component, st *cloudv1.ComponentStatus) {
	log := s.logger().With("cluster_id", row.ID, "cluster", row.Name, "tenant_id", row.TenantID,
		"component_id", c.ID, "component", c.Component, "version", st.GetTargetVersion())
	switch st.GetPhase() {
	case cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED:
		log.WarnContext(ctx, "component failed", "message", st.GetMessage(), "chart", st.GetChart(),
			"helm_state", st.GetHelmState(), "failures", st.GetFailures())
	case cloudv1.ComponentPhase_COMPONENT_PHASE_READY:
		log.InfoContext(ctx, "component ready")
	case cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING:
		log.InfoContext(ctx, "component installing")
	case cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING:
		log.InfoContext(ctx, "component being removed")
	default: // waiting is not news: the status says what for
	}
}

// removeTenant removes a disabled component from its cluster and reports
// whether it is gone: its charts are uninstalled (a chart before the one
// it was installed after, by the Usages of PlatformSet), then its role,
// then the Component itself. When the workload cluster cannot be reached
// (its EKS cluster is not ACTIVE, or the cluster's deletion is forced)
// the Releases are let go without an uninstall (forceRelease).
func (s *Reconciler) removeTenant(ctx context.Context, row *model.Cluster, c model.Component) (bool, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerComponent, c.ID)
	if err != nil {
		return false, err
	}
	if len(records) == 0 {
		return true, s.forgetComponent(ctx, row, c)
	}
	if err := s.setTenantStatus(ctx, row, c, &cloudv1.PlatformComponent{Name: c.Component,
		Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING, Message: "removing its charts and role"}, tenantStatus(c).GetRelease()); err != nil {
		return false, err
	}
	clusterRecords, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return false, err
	}
	active, err := s.eksActive(ctx, clusterRecords)
	if err != nil {
		return false, err
	}
	if !active || row.DeleteForce {
		if err := s.forceRelease(ctx, row); err != nil {
			return false, err
		}
	}
	owner := reconcile.Owner{TenantID: row.TenantID, CloudAccountID: row.CloudAccountID, Kind: model.OwnerComponent, ID: c.ID}
	if _, err := s.deps.Sequence().Delete(ctx, s.deps.Inventory(owner), crossplaneaws.PlatformDeleteOrder); err != nil {
		return false, deleteError(err)
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return false, err
	}
	if err := s.deps.MarkGone(ctx, records, views); err != nil {
		return false, err
	}
	if err := s.deps.ReleaseOrphanUsages(ctx, records, views); err != nil {
		return false, err
	}
	if !reconcile.AllGone(views) {
		return false, nil
	}
	return true, s.forgetComponent(ctx, row, c)
}

// forgetComponent removes a component that has nothing left on its
// cluster from the store.
func (s *Reconciler) forgetComponent(ctx context.Context, row *model.Cluster, c model.Component) error {
	if err := s.deps.Store.Components().Delete(ctx, c.TenantID, c.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("remove component %s: %w", c.Component, err)
	}
	s.logger().InfoContext(ctx, "component removed", "cluster_id", row.ID, "tenant_id", row.TenantID,
		"component_id", c.ID, "component", c.Component)
	return nil
}

// deleteTenant removes every tenant component of a cluster being deleted,
// before anything of the platform's or the cluster's own: each is marked
// as being removed (the API refuses to change it from then on) and
// removed as a disable removes it. It reports whether they are all gone.
func (s *Reconciler) deleteTenant(ctx context.Context, row *model.Cluster) (bool, error) {
	rows, err := s.deps.Store.Components().ListByCluster(ctx, row.TenantID, row.ID)
	if err != nil {
		return false, fmt.Errorf("list the cluster's components: %w", err)
	}
	all := true
	var errs []error
	for _, c := range rows {
		if c.State != componentDeleting {
			c.State = componentDeleting
			if err := s.deps.Store.Components().Update(ctx, &c); err != nil {
				errs = append(errs, fmt.Errorf("mark component %s for removal: %w", c.Component, err))
				all = false
				continue
			}
		}
		gone, err := s.removeTenant(ctx, row, c)
		if err != nil {
			errs = append(errs, err)
		}
		all = all && gone
	}
	return all && len(errs) == 0, errors.Join(errs...)
}
