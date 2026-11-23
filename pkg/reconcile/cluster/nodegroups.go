package cluster

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/clustersummary"
	"github.com/yindia/cluster-vending/pkg/component"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Bounds of what a pass asks of a workload cluster: reading or applying
// node groups never slows or fails the cluster's own pass.
const (
	nodeGroupCallTimeout  = 10 * time.Second
	nodeGroupTotalTimeout = 45 * time.Second
	nodeListLimit         = 500
	nodeGroupMessageMax   = 200
	// karpenterComponent is the platform component whose Ready phase
	// releases the node groups.
	karpenterComponent = component.KarpenterComponent
	// legacyPlatformPool is the name of the Karpenter NodePool and
	// EC2NodeClass an earlier build applied for the platform's components.
	// The system node group runs them now; what is left of it in a
	// workload cluster is removed once nothing needs it (legacyPoolGate).
	legacyPlatformPool = "platform"
)

// nodeGroup is a user node group as the status names it.
type nodeGroup struct {
	Name string
}

// nodeGroupsOf are the spec's user node groups, in order. The system node
// group, the platform's only one, is not among them (withNodeGroups).
func nodeGroupsOf(spec *cloudv1.ClusterSpec) []nodeGroup {
	out := make([]nodeGroup, 0, len(spec.GetNodeGroups()))
	for _, g := range spec.GetNodeGroups() {
		out = append(out, nodeGroup{Name: g.GetName()})
	}
	return out
}

func objectRef(u *unstructured.Unstructured) string {
	return strings.ToLower(u.GetKind()) + "/" + u.GetName()
}

// kubeletView is what a pass saw of the Karpenter nodes' kubelets, for the
// upgrade's node groups step.
type kubeletView struct {
	// known: nodes and NodeClaims were read completely.
	known bool
	// minors are the kubelet minors the nodes run ("1.37"); launching the
	// NodeClaims that have no node yet.
	minors    []string
	launching int
}

// nodeGroupsResult is what a pass learned of the cluster's node groups.
type nodeGroupsResult struct {
	// groups are the user groups' statuses.
	groups []*cloudv1.NodeGroupStatus
	// platformPool is the platform pool's status (PROP-03); nil: there is
	// no pool configured, or nothing is known of it.
	platformPool *cloudv1.NodeGroupStatus
	kubelets     kubeletView
	// system is what the pass saw of the default managed node group's nodes
	// and of the move off the old platform pool; zero: nothing, the
	// recorded values stay.
	system systemObserved
}

// systemObserved is what a pass observed of the system node group beyond its
// managed resource.
type systemObserved struct {
	// nodes is the count of its nodes (the nodes with the node group's
	// label); counted: the node list was read completely.
	nodes   int32
	counted bool
	// notice is why the node list could not be stored; noticeSet: this
	// pass tried (else the recorded notice stays).
	notice    string
	noticeSet bool
	// move is the message of the move off the old platform pool, empty
	// when there is none.
	move string
}

type nodeGroupsKey struct{}

type nodeGroupsHolder struct{ r *nodeGroupsResult }

func withNodeGroupsHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, nodeGroupsKey{}, &nodeGroupsHolder{})
}

func nodeGroupsOfPass(ctx context.Context) *nodeGroupsResult {
	if h, ok := ctx.Value(nodeGroupsKey{}).(*nodeGroupsHolder); ok {
		return h.r
	}
	return nil
}

// withNodeGroups puts the node groups into a status being built: the
// system group first (its phase from the node group's managed resource in
// st, its node count from the pass or, failing that, the recorded status),
// then the user groups this pass found, else the ones recorded.
func withNodeGroups(ctx context.Context, st, stored *cloudv1.ClusterStatus) {
	var observed systemObserved
	var user []*cloudv1.NodeGroupStatus
	var pool *cloudv1.NodeGroupStatus
	if r := nodeGroupsOfPass(ctx); r != nil {
		observed, user, pool = r.system, r.groups, r.platformPool
	} else {
		for _, g := range stored.GetNodeGroups() {
			// The system group is rebuilt below; the platform pool's record
			// stays; a platform group recorded by an earlier build is gone
			// with its pool.
			switch {
			case g.GetPlatformManaged() && g.GetName() == crossplaneaws.PoolPlatform:
				pool = g
			case !g.GetPlatformManaged():
				user = append(user, g)
			}
		}
	}
	platform := []*cloudv1.NodeGroupStatus{systemStatus(st, stored, observed)}
	if pool != nil {
		platform = append(platform, pool)
	}
	st.NodeGroups = slices.Concat(platform, user)
}

// systemStatus is the status of the default managed node group: PENDING,
// READY, FAILED or DELETING as its managed resource (the newest one while a
// replaced group is retired) is, with its message; the node count as the
// pass counted it, else as recorded.
func systemStatus(st, stored *cloudv1.ClusterStatus, observed systemObserved) *cloudv1.NodeGroupStatus {
	var current *cloudv1.ManagedResource
	for _, v := range st.GetManagedResources() {
		if v.GetKind() == KindNodeGroup {
			current = v
		}
	}
	phase, message := cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, "waiting for the node group to be created"
	if current != nil {
		message = current.GetMessage()
		switch current.GetState() {
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY:
			phase, message = cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, ""
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING:
			phase = cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING:
			phase = cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING
		default:
			if message == "" {
				message = "waiting for the node group to be Ready"
			}
		}
	}
	var nodes int32
	var notice string
	for _, g := range stored.GetNodeGroups() {
		if g.GetName() == systemNodeGroup {
			nodes, notice = g.GetNodes(), g.GetNodesNotice()
		}
	}
	if observed.counted {
		nodes = observed.nodes
	}
	if observed.move != "" {
		message = strings.TrimSpace(message + " " + observed.move)
	}
	if observed.noticeSet {
		notice = observed.notice
	}
	out := groupStatus(systemNodeGroup, true, phase, message, nodes)
	out.NodesNotice = notice
	return out
}

// hasKarpenterNodes: the recorded status has a user node group that was
// READY, so Karpenter's nodes follow an upgrade's target too.
func hasKarpenterNodes(row *model.Cluster) bool {
	for _, g := range storedStatus(row).GetNodeGroups() {
		if !g.GetPlatformManaged() && g.GetPhase() == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY {
			return true
		}
	}
	return false
}

func shortMessage(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= nodeGroupMessageMax {
		return s
	}
	return string([]rune(s)[:nodeGroupMessageMax-1]) + "…"
}

func groupStatus(name string, platform bool, phase cloudv1.NodeGroupPhase, message string, nodes int32) *cloudv1.NodeGroupStatus {
	return &cloudv1.NodeGroupStatus{Name: name, PlatformManaged: platform, Phase: phase, Message: shortMessage(message), Nodes: nodes}
}

// reconcileNodeGroups applies the cluster's user Karpenter node groups into
// its workload cluster (a cluster with none gets nothing applied), reads
// their status and the system group's nodes into the pass (nodeGroupsOfPass),
// and removes what an earlier build applied for the platform pool once
// nothing needs it. It runs after the platform components, and only once the
// Karpenter component is READY: nothing is applied before. It never fails the pass:
// what cannot be applied or read is the node group's status message, and a
// status that cannot be read keeps its last good phase.
func (s *Reconciler) reconcileNodeGroups(ctx context.Context, row *model.Cluster, installed *platform) {
	if s.deps.NodeGroupClients == nil || row.State == StateDeleting || noPlatform(row) {
		return
	}
	karpenter := cloudv1.ComponentPhase_COMPONENT_PHASE_UNSPECIFIED
	if installed != nil {
		found := false
		for _, c := range installed.components {
			if c.GetName() == karpenterComponent {
				karpenter, found = c.GetPhase(), true
			}
		}
		if !found && len(installed.components) > 0 {
			return // the catalog has no Karpenter: no node groups
		}
	}
	holder, _ := ctx.Value(nodeGroupsKey{}).(*nodeGroupsHolder)
	if holder == nil {
		return
	}
	spec, err := SpecOf(row)
	if err != nil {
		s.logger().WarnContext(ctx, "node groups: read spec", "cluster_id", row.ID, "error", err)
		return
	}
	groups := nodeGroupsOf(spec)
	stored := storedStatus(row).GetNodeGroups()
	if karpenter != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
		// Visible from the start: PENDING until Karpenter is Ready, the
		// recorded state kept for groups that already have one.
		pending := allGroups(groups, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, "waiting for Karpenter to be Ready")
		for i, g := range pending {
			for _, o := range stored {
				if o.GetName() == g.GetName() {
					pending[i] = o
				}
			}
		}
		holder.r = &nodeGroupsResult{groups: pending}
		if s.platformPoolOn() {
			holder.r.platformPool = recordedPool(stored, "waiting for Karpenter to be Ready")
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, nodeGroupTotalTimeout)
	defer cancel()
	holder.r = s.applyNodeGroups(ctx, row, spec, installed, groups, stored)
}

func allGroups(groups []nodeGroup, phase cloudv1.NodeGroupPhase, message string) []*cloudv1.NodeGroupStatus {
	out := make([]*cloudv1.NodeGroupStatus, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupStatus(g.Name, false, phase, message, 0))
	}
	return out
}

// degraded is every group's status when the workload cluster cannot be
// used: the last good phase and node count, with the reason as message.
func degraded(groups []nodeGroup, stored []*cloudv1.NodeGroupStatus, why string) *nodeGroupsResult {
	out := make([]*cloudv1.NodeGroupStatus, 0, len(groups))
	for _, g := range groups {
		phase, nodes := cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, int32(0)
		for _, o := range stored {
			if o.GetName() == g.Name {
				phase, nodes = o.GetPhase(), o.GetNodes()
			}
		}
		out = append(out, groupStatus(g.Name, false, phase, "status unavailable: "+why, nodes))
	}
	return &nodeGroupsResult{groups: out}
}

// waitingForIAM is every group's status while Karpenter's IAM is not ready
// for the node classes: nothing was applied, so a group that is not known
// to be anywhere yet is PENDING; one that has a recorded phase keeps it
// (what runs keeps running), with the wait as its message.
func waitingForIAM(groups []nodeGroup, stored []*cloudv1.NodeGroupStatus, what string) *nodeGroupsResult {
	out := make([]*cloudv1.NodeGroupStatus, 0, len(groups))
	for _, g := range groups {
		phase, nodes := cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, int32(0)
		for _, o := range stored {
			if o.GetName() == g.Name {
				phase, nodes = o.GetPhase(), o.GetNodes()
			}
		}
		out = append(out, groupStatus(g.Name, false, phase, "waiting for Karpenter's IAM ("+what+")", nodes))
	}
	return &nodeGroupsResult{groups: out}
}

// call runs f with its own short timeout.
func call(ctx context.Context, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, nodeGroupCallTimeout)
	defer cancel()
	return f(ctx)
}

func (s *Reconciler) applyNodeGroups(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec, installed *platform,
	groups []nodeGroup, stored []*cloudv1.NodeGroupStatus) *nodeGroupsResult {
	release := ""
	if installed != nil {
		release = installed.release
	}
	in, extra, err := s.nodeGroupsInput(ctx, row, spec, release)
	if err != nil {
		s.logger().WarnContext(ctx, "node groups: prepare", "cluster_id", row.ID, "error", err)
		return s.withPoolUnavailable(degraded(groups, stored, err.Error()), stored, err.Error())
	}
	// The node classes name Karpenter's node role: no user group is applied
	// before its IAM chain is Ready and the controller policy passes it, and
	// neither is the platform pool's. A cluster with neither needs none of it.
	if len(groups) > 0 || s.platformPoolOn() {
		wait, err := s.karpenterIAMWait(ctx, extra.namespace, in.ClusterPrefix, extra.accountID, spec.GetRegion())
		if err != nil {
			s.logger().WarnContext(ctx, "node groups: read Karpenter's IAM", "cluster_id", row.ID, "error", err)
			return s.withPoolUnavailable(degraded(groups, stored, err.Error()), stored, err.Error())
		}
		if wait != "" {
			res := waitingForIAM(groups, stored, wait)
			if s.platformPoolOn() {
				res.platformPool = recordedPool(stored, "waiting for Karpenter's IAM ("+wait+")")
			}
			return res
		}
	}
	var c client.Client
	err = call(ctx, func(ctx context.Context) (err error) {
		c, err = s.deps.NodeGroupClients.Client(ctx, kube.TenantNamespace(row.TenantID), storedStatus(row).GetKubeconfigSecret())
		return err
	})
	if err != nil {
		s.logger().WarnContext(ctx, "node groups: open the cluster", "cluster_id", row.ID, "error", err)
		s.noteUnreachable(ctx, row, err)
		return s.withPoolUnavailable(degraded(groups, stored, "cannot reach the cluster: "+err.Error()), stored, "cannot reach the cluster: "+err.Error())
	}
	failed := map[string]string{}
	objects, err := crossplaneaws.KarpenterNodeGroupObjects(in)
	if err != nil {
		return s.withPoolUnavailable(degraded(groups, stored, err.Error()), stored, err.Error())
	}
	for _, obj := range objects {
		if installed != nil && installed.holdPool && obj.GetName() == crossplaneaws.PoolPlatform {
			continue // its change waits its turn in the catalog change: what runs stays
		}
		err := call(ctx, func(ctx context.Context) error {
			return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner(kube.FieldOwner), client.ForceOwnership)
		})
		if err != nil {
			s.logger().WarnContext(ctx, "node groups: apply", "cluster_id", row.ID, "object", objectRef(obj), "error", err)
			if _, seen := failed[obj.GetName()]; !seen {
				failed[obj.GetName()] = describeApplyError(obj, err)
			}
		}
	}
	var usage *clustersummary.Aggregator
	if s.summaryDue(row.ID) {
		usage = clustersummary.NewAggregator()
	}
	nodes, kubelets := s.readNodesInto(ctx, c, extra.systemGroup, usage)
	s.summarize(ctx, c, row, nodes)
	legacy := &legacyPoolGate{s: s, c: c, row: row, installed: installed, extra: extra, nodes: nodes}
	removing := s.removeNodeGroups(ctx, c, row, groups, legacy, nodes)
	out := &nodeGroupsResult{kubelets: kubelets, system: systemObserved{nodes: nodes.count[systemNodeGroup], counted: nodes.complete,
		move: legacy.message}}
	if s.platformPoolOn() {
		out.platformPool = s.readPlatformPool(ctx, c, stored, failed[crossplaneaws.PoolPlatform], nodes)
	}
	for _, g := range groups {
		out.groups = append(out.groups, s.readGroup(ctx, c, g, stored, failed[g.Name], nodes))
	}
	for _, name := range removing {
		out.groups = append(out.groups, groupStatus(name, false, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING,
			"removed from the cluster; Karpenter drains its nodes", nodes.count[name]))
	}
	// The per-node rows go to the results bucket; the status keeps counts and,
	// when they cannot be stored, a notice.
	attempted, notice := s.writeNodeRows(ctx, row, nodes)
	withNodesNotice(out.groups, stored, attempted, notice)
	out.system.notice, out.system.noticeSet = notice, attempted
	withAMINotice(out.groups, extra.amiNotice)
	return out
}

// withAMINotice adds the AMI pin notice to the user groups' messages.
func withAMINotice(groups []*cloudv1.NodeGroupStatus, notice string) {
	if notice == "" {
		return
	}
	for _, g := range groups {
		if g.GetPhase() != cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING {
			g.Message = shortMessage(strings.TrimSpace(g.GetMessage() + " " + notice))
		}
	}
}

// legacyPoolGate decides whether the old platform pool of a cluster built
// before the system group took over may be removed
// (docs/architecture.md#node-layout, "Migration of existing clusters").
// The objects stay exactly as they are (not applied, not changed) until:
//
//  1. the system node group has its new placement and size and is Ready,
//     with that many Ready nodes;
//  2. every platform component is READY on a Release that is already what
//     the system placement renders (its Helm release settled: nothing is
//     changed in this pass) and every tenant component is READY;
//  3. no pod but a DaemonSet's (or a static one) is left on the old pool's
//     nodes, whatever component it belongs to: nothing still selects only
//     the old pool.
//
// message is what the system group's status says meanwhile; it is only
// asked when the cluster has such objects.
type legacyPoolGate struct {
	s         *Reconciler
	c         client.Client
	row       *model.Cluster
	installed *platform
	extra     nodeGroupsExtra
	nodes     nodeCounts

	decided, ok bool
	message     string
}

// allowed reports whether the old pool may be deleted now, deciding once per
// pass.
func (g *legacyPoolGate) allowed(ctx context.Context) bool {
	if g.decided {
		return g.ok
	}
	g.decided = true
	if why := g.systemNotReady(ctx); why != "" {
		g.message = "moving components off the old platform pool: waiting for the system node group (" + why + ")"
		return false
	}
	left := g.componentsLeft(ctx)
	if len(left) > 0 {
		g.message = fmt.Sprintf("moving components off the old platform pool: %d left (%s)", len(left), listNames(left))
		return false
	}
	g.ok = true
	g.message = "removing the old platform pool"
	return true
}

// systemNotReady is why the system node group is not ready to take over; ""
// when it is.
func (g *legacyPoolGate) systemNotReady(ctx context.Context) string {
	if g.extra.systemGroup == "" {
		return "its name is not known"
	}
	ng := &awseks.NodeGroup{}
	if err := g.s.getIgnoreMissing(ctx, client.ObjectKey{Namespace: g.extra.namespace, Name: g.extra.systemGroup}, ng); err != nil {
		return "cannot be read: " + err.Error()
	}
	at := ng.Status.AtProvider
	switch {
	case ptr.Deref(at.ID, "") == "" || ptr.Deref(at.Status, "") != "ACTIVE":
		return "not active yet"
	case at.Labels[crossplaneaws.PoolLabelKey] == nil || ptr.Deref(at.Labels[crossplaneaws.PoolLabelKey], "") != crossplaneaws.PoolSystem:
		return "its system label is not applied yet"
	case at.ScalingConfig == nil || int(ptr.Deref(at.ScalingConfig.DesiredSize, 0)) < g.extra.systemMin:
		return fmt.Sprintf("not at its minimum size of %d yet", g.extra.systemMin)
	}
	if !g.nodes.complete {
		return "its nodes could not be read"
	}
	ready := 0
	for _, n := range g.nodes.rows[systemNodeGroup] {
		if n.GetReady() {
			ready++
		}
	}
	if ready < g.extra.systemMin {
		return fmt.Sprintf("%d of %d nodes are Ready", ready, g.extra.systemMin)
	}
	return ""
}

// componentsLeft are the platform and tenant components that are not
// settled on the system placement: platform components not READY or still
// being changed, tenant components not READY, and the pods still on the old
// pool's nodes.
func (g *legacyPoolGate) componentsLeft(ctx context.Context) []string {
	var left []string
	if g.installed == nil {
		return []string{"platform components"}
	}
	for _, c := range g.installed.components {
		if c.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY || !c.GetUnchanged() {
			left = append(left, c.GetName())
		}
	}
	rows, err := g.s.deps.Store.Components().ListByCluster(ctx, g.row.TenantID, g.row.ID)
	if err != nil {
		return append(left, "tenant components")
	}
	for _, c := range rows {
		if c.State == componentDeleting {
			continue
		}
		if tenantStatus(c).GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY {
			left = append(left, c.Component)
		}
	}
	pods, err := g.podsOnOldPool(ctx)
	switch {
	case err != nil:
		left = append(left, "its pods could not be read")
	case pods > 0:
		left = append(left, fmt.Sprintf("%d pods on its nodes", pods))
	}
	return left
}

// podsOnOldPool counts the pods that run on the old pool's nodes and would
// not be everywhere anyway: not a DaemonSet's, not a static pod, not
// finished. The pod list is paginated and bounded by the pass deadline.
func (g *legacyPoolGate) podsOnOldPool(ctx context.Context) (int, error) {
	oldNodes := map[string]bool{}
	for _, n := range g.nodes.rows[legacyPlatformPool] {
		oldNodes[n.GetName()] = true
	}
	if len(oldNodes) == 0 {
		return 0, nil
	}
	count := 0
	err := eachPage(ctx, g.c, schema.GroupVersionKind{Version: "v1", Kind: "PodList"}, func(items []unstructured.Unstructured) {
		for i := range items {
			if oldNodes[nodeOfPod(&items[i])] && countsAsLeft(&items[i]) {
				count++
			}
		}
	})
	return count, err
}

func nodeOfPod(p *unstructured.Unstructured) string {
	name, _, _ := unstructured.NestedString(p.Object, "spec", "nodeName")
	return name
}

func countsAsLeft(p *unstructured.Unstructured) bool {
	if phase, _, _ := unstructured.NestedString(p.Object, "status", "phase"); phase == "Succeeded" || phase == "Failed" {
		return false
	}
	if _, mirror := p.GetAnnotations()["kubernetes.io/config.mirror"]; mirror {
		return false
	}
	for _, o := range p.GetOwnerReferences() {
		if o.Kind == "DaemonSet" {
			return false
		}
	}
	return true
}

func listNames(names []string) string {
	const shown = 5
	if len(names) <= shown {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
}

func describeApplyError(obj *unstructured.Unstructured, err error) string {
	if meta.IsNoMatchError(err) {
		return "Karpenter's CRDs are not installed yet"
	}
	return "apply " + objectRef(obj) + ": " + err.Error()
}

// NodeGroupObjects are the EC2NodeClasses and NodePools the controller
// applies to the cluster's workload cluster, rendered from the stored spec
// and status exactly as a pass renders them. None for a cluster whose
// platform is not managed (imported, not taken over) or whose catalog has
// no Karpenter. The API shows these.
func (s *Reconciler) NodeGroupObjects(ctx context.Context, row *model.Cluster) ([]*unstructured.Unstructured, error) {
	if noPlatform(row) {
		return nil, nil
	}
	comps := storedStatus(row).GetPlatformComponents()
	if len(comps) > 0 && !slices.ContainsFunc(comps, func(c *cloudv1.PlatformComponent) bool { return c.GetName() == karpenterComponent }) {
		return nil, nil
	}
	spec, err := SpecOf(row)
	if err != nil {
		return nil, err
	}
	in, _, err := s.nodeGroupsInput(ctx, row, spec, storedStatus(row).GetPlatformRelease())
	if err != nil {
		return nil, err
	}
	return crossplaneaws.KarpenterNodeGroupObjects(in)
}

// nodeGroupsInput are the node groups' shared values: the pinned AMI of
// the release's pin for the cluster's Kubernetes minor, and how subnets
// are found. release is the platform release the cluster follows ("": the
// default pin).
func (s *Reconciler) nodeGroupsInput(ctx context.Context, row *model.Cluster, spec *cloudv1.ClusterSpec, release string) (crossplaneaws.KarpenterNodeGroupsInput, nodeGroupsExtra, error) {
	in := crossplaneaws.KarpenterNodeGroupsInput{ClusterName: row.Name, ClusterPrefix: crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name),
		Discovery: true, NodeGroups: spec.GetNodeGroups(),
		DefaultArchitectures: s.deps.NodeGroups.Karpenter.Defaults.Architectures, DefaultCapacityTypes: s.deps.NodeGroups.Karpenter.Defaults.CapacityTypes,
		PlatformPool: s.platformPoolInput()}
	var rel *cloudv1alpha1.Release
	if src := s.releases(); src != nil && release != "" {
		var err error
		if rel, err = src.Release(ctx, release); err != nil {
			return in, nodeGroupsExtra{}, fmt.Errorf("read release %s: %w", release, err)
		}
	}
	var extra nodeGroupsExtra
	in.AMIPin, extra.amiNotice = resolveAMIPin(rel, nodeAMIMinor(spec.GetControlPlane().GetKubernetesVersion(), storedStatus(row)))
	t, err := s.Target(ctx, row.TenantID, spec)
	if err != nil {
		return in, extra, fmt.Errorf("read the network: %w", err)
	}
	extra.namespace, extra.accountID = t.Account.Namespace, t.Account.AccountID
	plan := s.RenderPlan(spec, row, t)
	extra.systemMin = plan.NodePool.Desired
	if name, _, err := s.NodeGroupName(ctx, row, plan); err == nil {
		extra.systemGroup = name
	}
	for _, name := range t.SubnetObjects {
		subnet := &awsec2.Subnet{}
		if err := s.deps.Get(ctx, client.ObjectKey{Namespace: t.Account.Namespace, Name: name}, subnet); err != nil {
			return in, extra, fmt.Errorf("read the network's subnet %s: %w", name, err)
		}
		if v := subnet.Spec.ForProvider.Tags[crossplaneaws.TagKarpenterDiscovery]; v != nil && *v != row.Name {
			in.Discovery = false
		}
	}
	return in, extra, nil
}

// nodeGroupsExtra is what nodeGroupsInput learned besides the input: the
// AMI pin notice for the status, and where Karpenter's IAM objects are.
type nodeGroupsExtra struct {
	amiNotice, namespace, accountID string
	// systemGroup is the default managed node group's object name and
	// systemMin the minimum size the render gives it (the autoscaler may add nodes above it) ("" and 0: not known).
	systemGroup string
	systemMin   int
}

// shippedAMIPins are the AMI pins this build's release.yaml ships, by
// Kubernetes minor; read once.
var shippedAMIPins = sync.OnceValue(func() map[string]string {
	out := map[string]string{}
	if rel, err := catalog.ShippedRelease(); err == nil {
		for _, k := range rel.Kubernetes {
			if k.NodeAMIPin != "" {
				out[k.Minor] = k.NodeAMIPin
			}
		}
	}
	return out
})

// resolveAMIPin is the AMI pin of the node classes on a minor: the
// cluster's recorded release's, else the one this build ships for the
// minor (a release published before nodeAmiPin existed must not move every
// node on each AWS AMI release), else "" (latest) with a notice for the
// status.
func resolveAMIPin(rel *cloudv1alpha1.Release, minor string) (pin, notice string) {
	if pin = amiPinOf(rel, minor); pin != "" {
		return pin, ""
	}
	if pin = shippedAMIPins()[minor]; pin != "" {
		return pin, ""
	}
	return "", fmt.Sprintf("no AMI pin for Kubernetes %s in the cluster's release or this build: nodes follow the latest AL2023 AMI", minor)
}

// amiPinOf is the release's AMI pin on a minor; "" when it has none.
func amiPinOf(rel *cloudv1alpha1.Release, minor string) string {
	if rel == nil {
		return ""
	}
	for _, k := range rel.Spec.Content.Kubernetes {
		if k.Minor == minor {
			return k.NodeAMIPin
		}
	}
	return ""
}

// nodeAMIMinor is the Kubernetes minor the node classes' AMI follows: the
// running one until an upgrade's control plane step is done, then the
// target's; the node groups step moves the nodes by moving the pin.
func nodeAMIMinor(specVersion string, stored *cloudv1.ClusterStatus) string {
	want := kubernetesMinor(specVersion)
	u := stored.GetUpgrade()
	if u == nil || !sameMinor(u.GetToVersion(), specVersion) {
		if running := kubernetesMinor(stored.GetCurrentKubernetesVersion()); behind(running, want) {
			return running
		}
		return want
	}
	if reached(stepOf(u), stepNodeGroups) {
		return want
	}
	if from := kubernetesMinor(u.GetFromVersion()); behind(from, want) {
		return from
	}
	return want
}

// removeNodeGroups deletes the platform's NodePools and EC2NodeClasses
// that no group wants any more (only objects with the managed-by label),
// and returns the names still there. A pool goes before its class. The
// objects of the old platform pool are left alone until legacy allows
// (nothing changes them, nothing is reported as a group); then its pool is
// deleted, and its class once its pool and its nodes are gone.
func (s *Reconciler) removeNodeGroups(ctx context.Context, c client.Client, row *model.Cluster, groups []nodeGroup,
	legacy *legacyPoolGate, nodes nodeCounts) []string {
	want := map[string]bool{}
	for _, g := range groups {
		want[g.Name] = true
	}
	if s.platformPoolOn() {
		want[crossplaneaws.PoolPlatform] = true // the platform's own pool, not a user group
	}
	var still []string
	pools := map[string]bool{}
	for _, gvk := range []struct {
		kind string
		pool bool
	}{{crossplaneaws.KarpenterNodePoolGVK.Kind, true}, {crossplaneaws.KarpenterNodeClassGVK.Kind, false}} {
		list := &unstructured.UnstructuredList{}
		if gvk.pool {
			list.SetGroupVersionKind(crossplaneaws.KarpenterNodePoolGVK.GroupVersion().WithKind("NodePoolList"))
		} else {
			list.SetGroupVersionKind(crossplaneaws.KarpenterNodeClassGVK.GroupVersion().WithKind("EC2NodeClassList"))
		}
		err := call(ctx, func(ctx context.Context) error {
			return c.List(ctx, list, client.MatchingLabels{crossplaneaws.LabelManagedBy: crossplaneaws.ManagedByPlatform}, client.Limit(nodeListLimit))
		})
		if err != nil {
			if !meta.IsNoMatchError(err) {
				s.logger().WarnContext(ctx, "node groups: list", "cluster_id", row.ID, "kind", gvk.kind, "error", err)
			}
			continue
		}
		for i := range list.Items {
			obj := &list.Items[i]
			name := obj.GetName()
			if want[name] {
				continue
			}
			old := name == legacyPlatformPool
			if old && !legacy.allowed(ctx) {
				continue
			}
			if !gvk.pool && (pools[name] || (old && nodes.count[name] > 0)) {
				if old {
					legacy.message = fmt.Sprintf("removing the old platform pool: waiting for Karpenter to terminate its nodes (%d left)", max(nodes.count[name], 1))
				} else {
					still = appendOnce(still, name)
				}
				continue // the class goes once its pool and nodes are gone
			}
			if gvk.pool {
				pools[name] = true
			}
			if err := call(ctx, func(ctx context.Context) error { return client.IgnoreNotFound(c.Delete(ctx, obj)) }); err != nil {
				s.logger().WarnContext(ctx, "node groups: delete", "cluster_id", row.ID, "object", objectRef(obj), "error", err)
			}
			if old {
				legacy.message = "removing the old platform pool: waiting for Karpenter to terminate its nodes"
			} else {
				still = appendOnce(still, name)
			}
		}
	}
	return still
}

func appendOnce(list []string, name string) []string {
	for _, n := range list {
		if n == name {
			return list
		}
	}
	return append(list, name)
}

// nodeCounts are the Karpenter nodes by node pool.
type nodeCounts struct {
	count map[string]int32
	// rows are the nodes by node group (the default managed group is
	// "system"), collected during the same reads; complete: the node lists
	// were read to the end. Written to the results bucket, never kept in the
	// Cluster.
	rows     map[string][]*cloudv1.NodeGroupNode
	complete bool
	// usage sums the nodes (and, once read, the pods) per group for the
	// cluster status summary; nil when the summary is not due.
	usage *clustersummary.Aggregator
	// err is why the node lists could not be read, nil when they were.
	err error
}

// eachPage lists kind with limit nodeListLimit and continue, calling f with
// each page's items, until every page was read. Only a page is held at a
// time. ctx is the pass deadline: when it ends, the read stops with its
// error and what f saw is partial. A kind the cluster does not have
// (Karpenter's CRDs are not there yet) is a no-match error.
func eachPage(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, f func([]unstructured.Unstructured), opts ...client.ListOption) error {
	token := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page := &unstructured.UnstructuredList{}
		page.SetGroupVersionKind(gvk)
		pageOpts := append(slices.Clone(opts), client.Limit(nodeListLimit), client.Continue(token))
		if err := call(ctx, func(ctx context.Context) error { return c.List(ctx, page, pageOpts...) }); err != nil {
			return err
		}
		f(page.Items)
		if token = page.GetContinue(); token == "" {
			return nil
		}
	}
}

// readNodes counts the Karpenter nodes per pool and reads their kubelet
// minors, with paginated lists (limit 500 and continue, however many nodes
// there are, bounded by the pass deadline); only counts are kept for the
// status. The same reads collect the per-node rows (nodeCounts.rows): the
// Karpenter nodes by pool, the default managed group's nodes as "system", each
// joined with its NodeClaim. A failed or unfinished read is "not known", and
// its rows are incomplete.
func (s *Reconciler) readNodes(ctx context.Context, c client.Client, systemGroup string) (nodeCounts, kubeletView) {
	return s.readNodesInto(ctx, c, systemGroup, nil)
}

// readNodesInto is readNodes that also sums the nodes into usage when it is
// not nil.
func (s *Reconciler) readNodesInto(ctx context.Context, c client.Client, systemGroup string, usage *clustersummary.Aggregator) (nodeCounts, kubeletView) {
	counts := nodeCounts{count: map[string]int32{}, rows: map[string][]*cloudv1.NodeGroupNode{}, usage: usage}
	view := kubeletView{}
	minors := map[string]bool{}
	byNode := map[string]*cloudv1.NodeGroupNode{}
	nodeList := schema.GroupVersionKind{Version: "v1", Kind: "NodeList"}
	err := eachPage(ctx, c, nodeList, func(items []unstructured.Unstructured) {
		for i := range items {
			n := &items[i]
			pool := n.GetLabels()[crossplaneaws.KarpenterNodePoolLabel]
			counts.count[pool]++
			row := nodeRowOf(n)
			counts.rows[pool] = append(counts.rows[pool], row)
			byNode[n.GetName()] = row
			if usage != nil {
				usage.AddNodeObject(pool, n)
			}
			if v, _, _ := unstructured.NestedString(n.Object, "status", "nodeInfo", "kubeletVersion"); v != "" {
				minors[kubernetesMinor(strings.TrimPrefix(v, "v"))] = true
			}
		}
	}, client.HasLabels{crossplaneaws.KarpenterNodePoolLabel})
	if err != nil {
		s.logger().WarnContext(ctx, "node groups: list nodes", "error", err)
		return nodeCounts{count: map[string]int32{}, rows: counts.rows, usage: usage, err: err}, kubeletView{}
	}
	counts.complete = true
	// The default managed node group's nodes are the "system" group: the
	// nodes labelled with its name (any managed group's, where it is not
	// known).
	var managed client.ListOption = client.HasLabels{managedNodeGroupLabel}
	if systemGroup != "" {
		managed = client.MatchingLabels{managedNodeGroupLabel: systemGroup}
	}
	err = eachPage(ctx, c, nodeList, func(items []unstructured.Unstructured) {
		for i := range items {
			counts.count[systemNodeGroup]++
			counts.rows[systemNodeGroup] = append(counts.rows[systemNodeGroup], nodeRowOf(&items[i]))
			if usage != nil {
				usage.AddNodeObject(systemNodeGroup, &items[i])
			}
		}
	}, managed)
	if err != nil {
		s.logger().WarnContext(ctx, "node groups: list managed nodes", "error", err)
		counts.complete = false
		counts.err = err
	}
	for m := range minors {
		view.minors = append(view.minors, m)
	}
	err = eachPage(ctx, c, crossplaneaws.KarpenterNodeClaimGVK.GroupVersion().WithKind("NodeClaimList"), func(items []unstructured.Unstructured) {
		for i := range items {
			name, _, _ := unstructured.NestedString(items[i].Object, "status", "nodeName")
			if name == "" {
				view.launching++
			} else if row := byNode[name]; row != nil {
				claimInto(row, &items[i])
			}
		}
	})
	switch {
	case err == nil:
		view.known = true
	case meta.IsNoMatchError(err):
		view.known = true
	default:
		s.logger().WarnContext(ctx, "node groups: list node claims", "error", err)
		counts.complete = false
	}
	return counts, view
}

// readGroup is a group's status: its NodePool's Ready condition and node
// count (unstructured, so extra fields, missing ones and any order of
// conditions are fine), or why that is not known.
func (s *Reconciler) readGroup(ctx context.Context, c client.Client, g nodeGroup, stored []*cloudv1.NodeGroupStatus, applyFailure string, nodes nodeCounts) *cloudv1.NodeGroupStatus {
	if applyFailure != "" {
		return groupStatus(g.Name, false, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED, applyFailure, nodes.count[g.Name])
	}
	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(crossplaneaws.KarpenterNodePoolGVK)
	err := call(ctx, func(ctx context.Context) error { return c.Get(ctx, client.ObjectKey{Name: g.Name}, pool) })
	switch {
	case err == nil:
	case meta.IsNoMatchError(err) || apierrors.IsNotFound(err):
		return groupStatus(g.Name, false, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, "waiting for Karpenter to report the node group", 0)
	default:
		return degraded([]nodeGroup{g}, stored, err.Error()).groups[0]
	}
	n := nodes.count[g.Name]
	if v, _, _ := unstructured.NestedString(pool.Object, "status", "resources", "nodes"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 32); err == nil {
			n = int32(parsed)
		}
	}
	ready, message := readyCondition(pool)
	if ready {
		return groupStatus(g.Name, false, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, "", n)
	}
	if message == "" {
		message = "waiting for Karpenter to report the node group"
	}
	return groupStatus(g.Name, false, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, message, n)
}

// readyCondition is whether the object's Ready condition is True and,
// when it is not, its message.
func readyCondition(obj *unstructured.Unstructured) (bool, string) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok || c["type"] != "Ready" {
			continue
		}
		if c["status"] == "True" {
			return true, ""
		}
		msg, _ := c["message"].(string)
		if msg == "" {
			msg, _ = c["reason"].(string)
		}
		return false, msg
	}
	return false, ""
}
