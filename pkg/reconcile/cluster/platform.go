package cluster

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"

	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/convert"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// ConditionPlatformComponents is the condition of ClusterStatus that says
// whether every component the platform installs with the cluster is
// installed. The cluster's state does not depend on it.
const ConditionPlatformComponents = cloudv1alpha1.ConditionPlatformComponentsReady

// Reasons of ConditionPlatformComponents.
const (
	reasonPlatformInstalled  = "Installed"
	reasonPlatformInstalling = "Installing"
	reasonPlatformFailed     = "Failed"
)

// platform is what a pass found out about a cluster's platform
// components: what the status reports.
type platform struct {
	release    string
	components []*cloudv1.PlatformComponent
	// move is the release move (release_move.go); nil when component
	// upgrades are off.
	move *cloudv1.ClusterReleaseMove
	// catalog is the catalog change (catalog_change.go); nil: the
	// recorded one stays.
	catalog *cloudv1.CatalogChange
	// holdPool: the platform pool's change waits its turn in the catalog
	// change, so the node pass does not apply it.
	holdPool bool
}

// storedStatus decodes the status recorded with the row; empty when there
// is none this build can read.
func storedStatus(row *model.Cluster) *cloudv1.ClusterStatus {
	st := &cloudv1.ClusterStatus{}
	if len(row.Status) > 0 {
		if err := Lenient.Unmarshal(row.Status, st); err != nil {
			return &cloudv1.ClusterStatus{}
		}
	}
	return st
}

// startedPlatform are the platform components the row's recorded status
// says something was installed for (by name): any phase but PENDING, and
// FAILED only once a chart was started.
func startedPlatform(row *model.Cluster) map[string]bool {
	out := map[string]bool{}
	for _, c := range storedStatus(row).GetPlatformComponents() {
		switch c.GetPhase() {
		case cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING, cloudv1.ComponentPhase_COMPONENT_PHASE_UNSPECIFIED:
		case cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED:
			out[c.GetName()] = c.GetChart() != ""
		default:
			out[c.GetName()] = true
		}
	}
	return out
}

// storedPlatform is what the row's recorded status says about the
// platform components: what a read returns, and what a pass that could
// not look reports again.
func storedPlatform(row *model.Cluster) *platform {
	st := storedStatus(row)
	return &platform{release: st.GetPlatformRelease(), components: st.GetPlatformComponents(), move: st.GetReleaseMove(),
		catalog: st.GetCatalogChange()}
}

// PlatformPending reports whether the row's recorded status says platform
// components are still being installed: the cluster is looked at again
// soon although it is RUNNING.
func PlatformPending(row *model.Cluster) bool {
	for _, c := range storedStatus(row).GetConditions() {
		if c.GetType() == ConditionPlatformComponents {
			return c.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE && c.GetReason() == reasonPlatformInstalling
		}
	}
	return false
}

// withPlatform writes the platform components and their condition into
// st.
func withPlatform(st *cloudv1.ClusterStatus, p *platform) {
	if p == nil {
		return
	}
	st.PlatformRelease, st.PlatformComponents, st.ReleaseMove = p.release, p.components, p.move
	if p.catalog != nil {
		st.CatalogChange = p.catalog
	}
	if len(p.components) == 0 {
		return
	}
	cond := &cloudv1.Condition{Type: ConditionPlatformComponents, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
		Reason: reasonPlatformInstalled, Message: fmt.Sprintf("%d components installed", len(p.components))}
	var failed, pending []string
	for _, c := range p.components {
		switch c.GetPhase() {
		case cloudv1.ComponentPhase_COMPONENT_PHASE_READY:
		case cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED:
			failed = append(failed, c.GetName())
		default:
			pending = append(pending, c.GetName())
		}
	}
	switch {
	case len(pending) > 0:
		cond.Status, cond.Reason = cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, reasonPlatformInstalling
		cond.Message = "installing " + strings.Join(pending, ", ")
		if len(failed) > 0 {
			cond.Message += "; failed: " + strings.Join(failed, ", ")
		}
	case len(failed) > 0:
		cond.Status, cond.Reason = cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, reasonPlatformFailed
		cond.Message = "failed: " + strings.Join(failed, ", ")
	}
	st.Conditions = append(st.Conditions, cond)
}

// installPlatform installs the components the platform runs on every
// cluster (the Platform definitions of the catalog) and reports them: it
// plans them from the catalog and the cluster's release
// (component.PlanPlatform), applies them in order
// (crossplaneaws.PlatformSet) and reads their state. Their objects are
// recorded apart from the cluster's own (model.OwnerClusterComponents), so
// the cluster's state never waits for them.
//
// Nothing is installed before the cluster can take Helm charts (its EKS
// cluster, a node group and its kubeconfig are Ready: platformCanStart,
// not RUNNING, which an add-on waiting for Karpenter's nodes would hold
// back for good), on an imported cluster that was not
// taken over (noPlatform; a taken-over one gets them like any cluster,
// unless its takeover kept them off), or while the cluster is being
// deleted. Once started it goes on through updates. nil
// when this pass has nothing new to say: the recorded status stays.
func (s *Reconciler) installPlatform(ctx context.Context, row *model.Cluster) (*platform, error) {
	if s.deps.Platform == nil || row.State == StateDeleting {
		return nil, nil
	}
	ctx = ensureHelmPass(ctx)
	if noPlatform(row) {
		// Imported and only observed, or taken over with the platform
		// components kept off: nothing is installed.
		return &platform{}, nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerClusterComponents, row.ID)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		start, err := s.platformCanStart(ctx, row)
		if err != nil || !start {
			return nil, err
		}
	}
	in, release, problem, err := s.platformInput(ctx, row, len(records) > 0, false)
	if err != nil {
		return nil, err
	}
	// The release move: this pass's pins while the
	// cluster moves to the release its channel offers, one component at a
	// time; the release recorded once it is done.
	var move *cloudv1.ClusterReleaseMove
	if problem == "" && len(records) > 0 {
		spec, err := SpecOf(row)
		if err != nil {
			return nil, err
		}
		if ctx, move, release, problem, err = s.moveRelease(ctx, row, spec, &in, release); err != nil {
			return nil, err
		}
	}
	if problem != "" {
		p := unavailable(in.Entries, release, problem)
		p.move, p.catalog = move, storedStatus(row).GetCatalogChange()
		s.describe(ctx, row, in.Plan.Namespace, p, nil)
		return p, nil
	}
	if len(records) == 0 {
		// The first install: the chosen release is stored before any Release
		// object exists, so no later pass (whatever happened to the status
		// write of this one) picks again what the channel offers by then.
		if err := s.pinRelease(ctx, row, release); err != nil {
			return nil, err
		}
	}
	plan, skipped := component.PlanPlatform(in)
	gate := s.gateOf(ctx, row, in.Plan.Namespace)
	// The catalog change (catalog_change.go): a definition whose change
	// waits its turn keeps what runs on the cluster.
	spec, err := SpecOf(row)
	if err != nil {
		return nil, err
	}
	catalog, err := s.catalogChange(ctx, row, spec, in, plan, move)
	if err != nil {
		return nil, fmt.Errorf("catalog change: %w", err)
	}
	if r := catalog.d.Recover; r != "" {
		ctx = withRetry(ctx, retryWith(row.Retry, r, now()))
	}
	plan = catalog.plan(plan)
	plan.ReleaseGate = catalog.releaseGate(gate, plan.ReleaseGate)
	owner := reconcile.Owner{TenantID: row.TenantID, CloudAccountID: row.CloudAccountID, Kind: model.OwnerClusterComponents, ID: row.ID}
	inventory := catalog.inventory(ctx, gate, in.Plan.ClusterPrefix, s.deps.Inventory(owner))
	if slices.ContainsFunc(skipped, func(c component.NotInstalled) bool { return c.Preserve }) {
		// A partial plan cannot prove deletion intent. Apply valid components but
		// retain all previous inventory until the entire plan is renderable.
		inventory = sequence.Keeping(inventory, func(sequence.Entry) bool { return true })
	}
	gate.register(catalog.registered(plan), false)
	res, err := s.deps.Sequence().Apply(ctx, crossplaneaws.PlatformSet(plan), inventory)
	if err != nil {
		s.logger().ErrorContext(ctx, "apply platform components", "cluster_id", row.ID, "error", err)
		return nil, fault.New(fault.Internal, "could not apply the cluster's platform components")
	}
	records, err = s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerClusterComponents, row.ID)
	if err != nil {
		return nil, err
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return nil, err
	}
	components, objects := platformStatus(in, plan, skipped, res, views)
	for _, pc := range components {
		if c, ok := plannedComponent(plan, pc.GetName()); ok {
			gate.markUnchanged(pc, plan, c)
		}
	}
	catalog.report(components)
	p := &platform{release: release, components: components, move: move, catalog: catalog.status(), holdPool: catalog.holdPool()}
	s.describe(ctx, row, in.Plan.Namespace, p, objects)
	explainTakenOver(row, p.components)
	return p, nil
}

// pinReleaseAttempts bounds the re-reads of pinRelease when the row changes
// under it.
const pinReleaseAttempts = 5

// pinRelease records release as the cluster's platform release in the stored
// status, on the row as it is now (the row of the pass may be older: a call
// that changed the spec meanwhile makes the write conflict and it is made
// again). An error leaves the first install for the next pass.
func (s *Reconciler) pinRelease(ctx context.Context, row *model.Cluster, release string) error {
	var err error
	for range pinReleaseAttempts {
		var cur *model.Cluster
		if cur, err = s.deps.Store.Clusters().Get(ctx, row.TenantID, row.ID); err != nil {
			return storeError(err)
		}
		st := storedStatus(cur)
		if st.GetPlatformRelease() == release {
			return nil
		}
		st.PlatformRelease = release
		raw, eerr := Encode(st)
		if eerr != nil {
			return eerr
		}
		if err = s.deps.Store.Clusters().SetStatus(ctx, cur.TenantID, cur.ID, cur.ETag, raw); err == nil {
			return nil
		}
		if !errors.Is(err, repository.ErrConflict) {
			return storeError(err)
		}
	}
	return storeError(err)
}

// plannedComponent is the plan's component of that name.
func plannedComponent(plan crossplaneaws.PlatformPlan, name string) (crossplaneaws.PlatformComponentPlan, bool) {
	for _, c := range plan.Components {
		if c.Name == name {
			return c, true
		}
	}
	return crossplaneaws.PlatformComponentPlan{}, false
}

// unavailable reports every Platform component as not installable for one
// reason (the release cannot be used).
func unavailable(entries []component.PlatformEntry, release, problem string) *platform {
	out := &platform{release: release}
	for _, e := range entries {
		out.components = append(out.components, &cloudv1.PlatformComponent{Name: e.Name,
			Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Message: problem})
	}
	return out
}

// platformStatus is the state of each Platform component, in install
// order: left out (FAILED, why), waiting (PENDING, what for), being
// installed (INSTALLING) or with every chart deployed (READY). A chart
// whose install failed, or a failed IAM prerequisite, makes its component
// FAILED. objects names, for each component that is not READY, the Release
// object its phase is about (the chart that failed, waits or is being installed).
func platformStatus(in component.PlatformInput, plan crossplaneaws.PlatformPlan, skipped []component.NotInstalled,
	res sequence.Result, views []*cloudv1.ManagedResource) (components []*cloudv1.PlatformComponent, objects map[string]string) {
	objects = map[string]string{}
	reasons := map[string]string{}
	for _, sk := range skipped {
		reasons[sk.Name] = sk.Reason
	}
	planned := map[string]crossplaneaws.PlatformComponentPlan{}
	for _, c := range plan.Components {
		planned[c.Name] = c
	}
	state := map[string]*cloudv1.ManagedResource{}
	for _, v := range views {
		if v.GetKind() == crossplaneaws.HelmReleaseGVK.Kind {
			state[v.GetName()] = v
		}
	}
	waiting := map[string][]string{}
	for _, w := range res.Waiting {
		if w.Ref.Kind == crossplaneaws.HelmReleaseGVK.Kind {
			waiting[w.Ref.Name] = w.For
		}
	}
	out := make([]*cloudv1.PlatformComponent, 0, len(in.Entries))
	for _, e := range in.Entries {
		pc := &cloudv1.PlatformComponent{Name: e.Name, Version: in.Pins[e.Name].Version}
		c, ok := planned[e.Name]
		if !ok {
			pc.Phase, pc.Message = cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, reasons[e.Name]
			out = append(out, pc)
			continue
		}
		if message := componentIAMFailure(c, views); message != "" {
			pc.Phase, pc.Message = cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, message
			out = append(out, pc)
			continue
		}
		var object string
		pc.Phase, pc.Message, object = componentPhase(plan.ReleaseObjectNames(c), state, waiting)
		if object != "" {
			objects[e.Name] = object
		}
		if missing := component.MissingCompanions(e, in.Pins); len(missing) > 0 && pc.Phase == cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			pc.Message = fmt.Sprintf("without %s: not in release %s", strings.Join(missing, ", "), in.Release)
		}
		out = append(out, pc)
	}
	return out, objects
}

// IAM failures belong to the component that rendered the exact object, not
// to a Helm Release. Keep objects empty so describe does not report IAM
// policy metadata as chart metadata. Missing or pending IAM is ordinary waiting.
func componentIAMFailure(c crossplaneaws.PlatformComponentPlan, views []*cloudv1.ManagedResource) string {
	if c.IAM == nil {
		return ""
	}
	for _, object := range crossplaneaws.ComponentIAMResources(*c.IAM) {
		kind := reflect.TypeOf(object).Elem().Name()
		for _, v := range views {
			if v.GetName() == object.GetName() && (v.GetKind() == "" || v.GetKind() == kind) &&
				v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED {
				return fmt.Sprintf("%s %s: %s", kind, v.GetName(), v.GetMessage())
			}
		}
	}
	return ""
}

// removingMessage is a platform component whose chart is being
// uninstalled: its definition was disabled, or it left the release.
const removingMessage = "being removed from the cluster"

// componentPhase reads a component's phase from its Release objects
// (names, in install order). object is the Release the phase is about: the
// one that failed or is being installed; "" when all are deployed or none
// was started.
func componentPhase(names []string, state map[string]*cloudv1.ManagedResource, waiting map[string][]string) (phase cloudv1.ComponentPhase, message, object string) {
	ready, started := 0, false
	for _, name := range names {
		v, recorded := state[name]
		if recorded && v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			started = true
		}
		switch {
		case recorded && v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED:
			return cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, fmt.Sprintf("chart %s: %s", v.GetExternalName(), v.GetMessage()), name
		case recorded && v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING:
			return cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING, removingMessage, name
		case len(waiting[name]) > 0:
			// Held back: not created yet, or (a change another Helm
			// operation of the cluster holds back) not READY until it is
			// written and rolled out, whatever the Release says now.
			phase := cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING
			if started {
				phase = cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING
			}
			return phase, "waiting for " + strings.Join(waiting[name], ", "), ""
		case recorded && v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY:
			ready++
		case recorded:
			return cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, v.GetMessage(), name
		default:
			return cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING, "", ""
		}
	}
	if ready == len(names) {
		return cloudv1.ComponentPhase_COMPONENT_PHASE_READY, "", ""
	}
	return cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING, "", ""
}

// platformInput gathers what the cluster's platform components are planned
// from. problem, when set, says why none can be installed now (the
// release cannot be used); the entries are still returned, to report.
func (s *Reconciler) platformInput(ctx context.Context, row *model.Cluster, installed, tenant bool) (in component.PlatformInput, release, problem string, err error) {
	spec, err := SpecOf(row)
	if err != nil {
		return in, "", "", err
	}
	t, err := s.Target(ctx, row.TenantID, spec)
	if err != nil {
		return in, "", "", err
	}
	defs, err := s.deps.Platform.Definitions(ctx)
	if err != nil {
		return in, "", "", fmt.Errorf("read the component catalog: %w", err)
	}
	in.Entries, err = component.PlatformEntries(defs)
	if err != nil {
		return in, "", "", fault.New(fault.FailedPrecondition, "the component catalog is inconsistent: "+err.Error())
	}
	// A cluster gets the definitions for its cloud. One installed already
	// whose definition is now for another cloud is kept: a catalog change
	// never uninstalls.
	var elsewhere []string
	in.Entries, elsewhere = component.ForProvider(in.Entries, string(row.Provider), startedPlatform(row))
	in.Entries = component.ForNaaS(in.Entries, naasForPass(spec.GetNaas(), row, in.Entries))
	if len(elsewhere) > 0 {
		s.logger().WarnContext(ctx, "platform components kept although their definitions are for another cloud",
			"cluster_id", row.ID, "provider", row.Provider, "components", elsewhere)
	}
	clusterPlan := s.RenderPlan(spec, row, t)
	in.Plan = crossplaneaws.PlatformPlan{Namespace: clusterPlan.Namespace, TenantID: row.TenantID, ClusterID: row.ID,
		ClusterPrefix: clusterPlan.Prefix, ReleaseGate: s.gateOf(ctx, row, clusterPlan.Namespace).decide}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return in, "", "", err
	}
	if groups := nodeGroupRecords(records); len(groups) > 0 {
		in.Plan.NodeGroupName = groups[len(groups)-1].Name
	}
	in.IAM = crossplaneaws.ComponentIAMPlan{Namespace: clusterPlan.Namespace, ProviderConfigName: clusterPlan.ProviderConfigName,
		TenantID: row.TenantID, ClusterID: row.ID, ClusterName: row.Name, ClusterPrefix: clusterPlan.Prefix,
		Region: spec.GetRegion(), Tags: row.Labels}
	in.Cluster = component.Cluster{Name: row.Name, State: row.State, Running: row.State == StateRunning, Provider: string(row.Provider),
		KubernetesMinor: componentsMinor(spec.GetControlPlane().GetKubernetesVersion(), storedStatus(row), tenant),
		// The provider spec.aws.irsa renders, or the one a taken-over
		// cluster was imported with.
		IRSA: HasIRSA(spec, records)}
	in.Vars = s.platformVars(ctx, row, spec, t, clusterPlan, in.IAM, in.Cluster.IRSA, in.Plan.NodeGroupName, installed)

	// The recorded release is kept once something is installed from it.
	// Until then the cluster follows its channel: a release that pinned no
	// component must not hold the cluster for ever.
	if installed {
		release = storedStatus(row).GetPlatformRelease()
	}
	if release != "" {
		// A kept release that is gone, or not valid any more (the catalog
		// changed under it), cannot be installed from: the cluster leaves
		// it for what its channel offers.
		kept, err := s.deps.Platform.Release(ctx, release)
		if err != nil {
			return in, release, "", fmt.Errorf("read release %s: %w", release, err)
		}
		if kept == nil || meta.IsStatusConditionFalse(kept.Status.Conditions, cloudv1alpha1.ReleaseConditionValid) {
			release = ""
		}
	}
	if release == "" {
		channel := channelOf(spec)
		if release, err = s.deps.Platform.Offered(ctx, channel); err != nil {
			return in, "", "", fmt.Errorf("read release channel %s: %w", channel, err)
		}
		if release == "" {
			return in, "", fmt.Sprintf("release channel %s offers no release yet", channel), nil
		}
	}
	in.Release = release
	rel, err := s.deps.Platform.Release(ctx, release)
	if err != nil {
		return in, release, "", fmt.Errorf("read release %s: %w", release, err)
	}
	if rel == nil {
		return in, release, fmt.Sprintf("release %s does not exist", release), nil
	}
	in.Pins, problem = pinsOf(rel, in.Cluster.KubernetesMinor)
	return in, release, problem, nil
}

// pinsOf are the release's pins on the Kubernetes minor, by name; problem
// says why the release cannot be used for it.
func pinsOf(rel *cloudv1alpha1.Release, minor string) (pins map[string]component.Pin, problem string) {
	for _, k := range rel.Spec.Content.Kubernetes {
		if k.Minor != minor {
			continue
		}
		pins = map[string]component.Pin{}
		for _, c := range k.Components {
			values, err := component.PinValues(c.Values)
			if err != nil {
				return nil, fmt.Sprintf("release %s has invalid values for %s: %v", rel.Name, c.Name, err)
			}
			pins[c.Name] = component.Pin{Version: c.Version, Values: values}
		}
		return pins, ""
	}
	return nil, fmt.Sprintf("release %s does not offer Kubernetes %s", rel.Name, minor)
}

// channelOf is the name of the release channel the cluster takes its
// platform components from: its own, or stable when it names none.
func channelOf(spec *cloudv1.ClusterSpec) string {
	switch spec.GetReleaseChannel() {
	case cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA:
		return cloudv1alpha1.ChannelAlpha
	case cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA:
		return cloudv1alpha1.ChannelBeta
	default:
		return cloudv1alpha1.ChannelStable
	}
}

// componentsMinor is the Kubernetes minor the components are planned for
// (their pins and values, e.g. an image that follows the minor): during an
// upgrade the minor they run (the upgrade's from_version) until the
// upgrade reaches their step (the platform components', for tenant false;
// the tenant components', for tenant true), the spec's then and when no
// upgrade is recorded. A spec ahead of the version last recorded for the
// cluster with no upgrade recorded yet (it was just asked for) keeps that
// version's minor.
func componentsMinor(specVersion string, stored *cloudv1.ClusterStatus, tenant bool) string {
	want := kubernetesMinor(specVersion)
	u := stored.GetUpgrade()
	if u == nil || !sameMinor(u.GetToVersion(), specVersion) {
		running := kubernetesMinor(stored.GetCurrentKubernetesVersion())
		if behind(running, want) {
			return running
		}
		return want
	}
	step := stepPlatformComps
	if tenant {
		step = stepTenantComps
	}
	if from := kubernetesMinor(u.GetFromVersion()); !reached(stepOf(u), step) && behind(from, want) {
		return from
	}
	return want
}

// behind reports whether minor have is older than minor want; false when
// either does not read as a minor.
func behind(have, want string) bool {
	haveMajor, haveMinor, okHave := minorParts(have)
	wantMajor, wantMinor, okWant := minorParts(want)
	return okWant && okHave && (haveMajor < wantMajor || haveMajor == wantMajor && haveMinor < wantMinor)
}

// minorParts are the numbers of a "1.37" minor; ok is false for anything
// else.
func minorParts(minor string) (major, number int, ok bool) {
	parts := strings.Split(minor, ".")
	if len(parts) != 2 {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	number, errMinor := strconv.Atoi(parts[1])
	return major, number, errMajor == nil && errMinor == nil
}

// kubernetesMinor is the Kubernetes minor of a version: "1.37" of "1.37" and of
// "1.37.2".
func kubernetesMinor(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// platformVars are a component's placeholder values: the cluster's, as
// its recorded status has them (endpoint, issuer), its network's VPC, its
// account, the system group's labels and taints, the platform pool's once
// it is Ready (platformPoolWait), and the component's own role and policy
// as the management cluster has them now. What does not exist yet is left
// empty: the component then waits.
func (s *Reconciler) platformVars(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec, t Target,
	clusterPlan crossplaneaws.ClusterPlan, iam crossplaneaws.ComponentIAMPlan, irsa bool, nodeGroup string, installed bool) func(string) component.Vars {
	recorded := storedStatus(row)
	kindSpec, _ := convert.ClusterSpecToAPI(spec, "")
	if irsa {
		// Also for the provider a taken-over cluster was imported with,
		// which its spec does not say (hasOIDCProvider).
		if kindSpec.AWS == nil {
			kindSpec.AWS = &cloudv1alpha1.AWSClusterSpec{}
		}
		kindSpec.AWS.IRSA = ptr.To(true)
	}
	cluster := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: row.Name}, Spec: kindSpec}
	cluster.Status.OIDCIssuerURL = recorded.GetOidcIssuerUrl()
	if e := recorded.GetEndpoint(); e.GetUrl() != "" {
		cluster.Status.Endpoint = &cloudv1alpha1.ClusterEndpoint{URL: e.GetUrl()}
	}
	account := &cloudv1alpha1.CloudAccount{Spec: cloudv1alpha1.CloudAccountSpec{AWS: &cloudv1alpha1.AWSAccount{AccountID: t.Account.AccountID}}}
	network := &cloudv1alpha1.Network{}
	if vpc := vpcOf(t.Network); vpc != "" {
		network.Status.Resources = &cloudv1alpha1.NetworkResources{CloudNetworkID: vpc}
	}
	nodeRole := crossplaneaws.NodeRoleName(clusterPlan)
	live, liveErr := s.livePlacement(ctx, cluster, clusterPlan.Namespace, nodeGroup, installed)
	poolWait := s.platformPoolWait(ctx, row, cluster.Spec.ImportFrom != nil)
	return func(name string) component.Vars {
		p := iam
		p.Component = name
		out := component.IAMOutputs{RoleName: crossplaneaws.ComponentRoleName(p), NodeRoleName: nodeRole, ClusterPrefix: clusterPlan.Prefix}
		role := &awsiam.Role{}
		if err := s.getIgnoreMissing(ctx, client.ObjectKey{Namespace: p.Namespace, Name: out.RoleName}, role); err == nil {
			out.RoleARN = ptr.Deref(role.Status.AtProvider.Arn, "")
		}
		policy := &awsiam.Policy{}
		if err := s.getIgnoreMissing(ctx, client.ObjectKey{Namespace: p.Namespace, Name: crossplaneaws.ComponentPolicyName(p)}, policy); err == nil {
			out.PolicyARN = ptr.Deref(policy.Status.AtProvider.Arn, "")
		}
		vars := component.VarsFor(cluster, network, account, out)
		if live != nil {
			vars = component.WithPlacement(vars, *live)
		}
		vars = s.withPlatformPool(vars, live, poolWait)
		if liveErr != nil {
			// The node group cannot be read: the system placement is not
			// known (nor the platform one, where it is the system's), so
			// the components that use it wait rather than move.
			names := []string{component.VarSystemNodeSelector, component.VarSystemTolerations}
			if !s.platformPoolOn() {
				names = append(names, component.VarPlatformNodeSelector, component.VarPlatformTolerations)
			}
			for _, n := range names {
				delete(vars, n)
			}
		}
		return vars
	}
}

// vpcOf is the network's VPC ID as its recorded status has it.
func vpcOf(n *model.Network) string {
	st := &cloudv1.NetworkStatus{}
	if n == nil || len(n.Status) == 0 || Lenient.Unmarshal(n.Status, st) != nil {
		return ""
	}
	return st.GetResources().GetCloudNetworkId()
}
