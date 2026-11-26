package cluster

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/helmrel"
)

// helmGate is the cluster's Helm operations as one pass sees them
// (docs/architecture.md#helm-operations, mechanisms 2 and 3):
//
//   - a Release whose spec digest equals the live one is not written;
//   - a new Release waits until the Releases it comes after rolled out:
//     the charts before it of its component and those of the components
//     it requires are Ready (rollout: Helm deployed them and their
//     workloads are available);
//   - at most one Release of the cluster is installing, upgrading or
//     rolling out (deployed, its workloads not available yet): a
//     Release that must change waits while another has an operation in
//     flight (running, within its timeout), or one was written earlier in
//     this pass. A Release given up, stuck past its timeout, in its
//     backoff or paused by anyone is no operation in flight and blocks
//     nobody. Only a Release that is itself unsettled may be changed,
//     which is how a new digest ends a failing install's backoff;
//   - install order holds: a Release never waits behind one of a higher
//     sequence, or one that requires its component, and while several must
//     change the lowest sequence (a required one first) goes first.
//
// The gate governs Helm Release objects only: it is the Gate of a
// Release's item, never of a component's IAM objects.
//
// It reads the cluster's Releases once, by label, so a pass costs one list
// of the cluster's own releases, never a scan of the fleet. The platform's
// and the tenant's components of a pass share one gate.
type helmGate struct {
	s                 *Reconciler
	namespace         string
	tenantID, cluster string
	// kubeconfig is the cluster's kubeconfig Secret, to read its
	// workloads.
	kubeconfig string

	once sync.Once
	err  error
	// live are the cluster's Releases by object name, in a stable order.
	live  map[string]*releaseDetail
	names []string

	mu sync.Mutex
	// started is the Release this pass wrote first, "" for none.
	started string
	// unchanged are the Releases whose digest was already the live one.
	unchanged map[string]bool
	// items are the Release objects the pass's plans render, by object
	// name; requires are the components' requirements (all plans of the
	// pass).
	items    map[string]gateItem
	requires map[string][]string
	// order are each component's Release objects in install order.
	order map[string][]string

	// rmu guards the pass's rollout reads (helm_rollout.go): the
	// readiness found per Release, digest and revision, and the reader of
	// the cluster's workloads.
	rmu       sync.Mutex
	rollouts  map[string]readiness
	workloads *workloads
	// written are the Releases this pass lets be written: what the
	// management cluster shows of them is about the spec before.
	written map[string]bool
}

// gateItem is a Release a plan renders: its component, where that comes in
// the install order, and the digest it must have.
type gateItem struct {
	component string
	rank      int32
	want      string
	// waits: the component's values wait for something, so the Release
	// cannot be written now.
	waits bool
}

// tenantRank puts tenant components after every platform one: they wait
// for all of them.
const tenantRank = int32(1<<31 - 1)

// register tells the gate what a plan renders, before the plan is applied:
// the install order (sequence, requires) and the digests that must change.
func (g *helmGate) register(plan crossplaneaws.PlatformPlan, tenant bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.items == nil {
		g.items, g.requires, g.order = map[string]gateItem{}, map[string][]string{}, map[string][]string{}
	}
	for _, c := range plan.Components {
		rank := c.Sequence
		if tenant {
			rank = tenantRank
		}
		g.requires[c.Name] = c.Requires
		g.order[c.Name] = plan.ReleaseObjectNames(c)
		for name, digest := range plan.ReleaseDigests(c) {
			g.items[name] = gateItem{component: c.Name, rank: rank, want: digest, waits: c.WaitsFor != ""}
		}
	}
}

type helmPassKey struct{}

// helmPass lets the steps of one pass share a gate.
type helmPass struct {
	mu   sync.Mutex
	gate *helmGate
}

// withHelmPass gives a pass the place its steps share their gate in.
func withHelmPass(ctx context.Context) context.Context {
	return context.WithValue(ctx, helmPassKey{}, &helmPass{})
}

// ensureHelmPass is ctx with a place for the pass's gate; ctx itself when
// it has one (Reconcile gave it).
func ensureHelmPass(ctx context.Context) context.Context {
	if _, ok := ctx.Value(helmPassKey{}).(*helmPass); ok {
		return ctx
	}
	return withHelmPass(ctx)
}

// gateOf is the pass's gate for the cluster; a pass without a place to
// share it (a step called alone) gets a gate of its own.
func (s *Reconciler) gateOf(ctx context.Context, row *model.Cluster, namespace string) *helmGate {
	if p, ok := ctx.Value(helmPassKey{}).(*helmPass); ok {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.gate == nil {
			p.gate = s.newGate(row, namespace)
		}
		return p.gate
	}
	return s.newGate(row, namespace)
}

func (s *Reconciler) newGate(row *model.Cluster, namespace string) *helmGate {
	return &helmGate{s: s, namespace: namespace, tenantID: row.TenantID, cluster: row.ID, unchanged: map[string]bool{},
		kubeconfig: storedStatus(row).GetKubeconfigSecret()}
}

// load reads the cluster's Releases, once.
func (g *helmGate) load(ctx context.Context) {
	g.once.Do(func() {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK.GroupVersion().WithKind(crossplaneaws.HelmReleaseGVK.Kind + "List"))
		if err := g.s.deps.Reader().List(ctx, list, client.InNamespace(g.namespace),
			client.MatchingLabels(crossplane.ClusterLabels(g.tenantID, g.cluster))); err != nil {
			g.err = err
			return
		}
		g.live = make(map[string]*releaseDetail, len(list.Items))
		for i := range list.Items {
			u := &list.Items[i]
			d := detailOf(u)
			d.deleting = !u.GetDeletionTimestamp().IsZero()
			d.component = u.GetLabels()[crossplane.LabelComponent]
			g.live[u.GetName()] = d
			g.names = append(g.names, u.GetName())
		}
		slices.Sort(g.names)
	})
}

// settled reports whether the live Release name holds nothing back: Ready
// (rollout: deployed and its workloads available), given up, paused by
// someone else (it does nothing), or going away.
func (g *helmGate) settled(ctx context.Context, name string, d *releaseDetail) bool {
	switch {
	case d.deleting, d.Paused, d.GaveUp:
		return true
	case d.behind:
		return false
	}
	return g.rollout(ctx, name, d).Ready
}

// decide is the platform plan's ReleaseGate.
func (g *helmGate) decide(ctx context.Context, release *unstructured.Unstructured) sequence.Decision {
	g.load(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return sequence.Decision{Hold: "the cluster's Helm Releases could not be read: " + g.err.Error()}
	}
	name := release.GetName()
	want := release.GetAnnotations()[helmrel.DigestAnnotation]
	live := g.live[name]
	if live != nil && live.Digest != "" && live.Digest == want {
		g.unchanged[name] = true
		return sequence.Decision{Skip: true}
	}
	// A change. It first waits for what comes before it in the install
	// order that must change too, then may start when nothing else of the
	// cluster is in the middle of an operation, or when it is that one.
	if first := g.firstToChange(name); first != "" {
		return sequence.Decision{Hold: g.holdMessage(first)}
	}
	if live == nil {
		if hold := g.notRolledOut(ctx, name); hold != "" {
			return sequence.Decision{Hold: hold}
		}
	}
	if live == nil || g.settled(ctx, name, live) {
		if busy := g.busy(ctx, name); busy != "" {
			return sequence.Decision{Hold: g.holdMessage(busy)}
		}
	}
	if g.started == "" {
		g.started = name
	}
	g.rmu.Lock()
	if g.written == nil {
		g.written = map[string]bool{}
	}
	g.written[name] = true
	g.rmu.Unlock()
	return sequence.Decision{}
}

// busy is the Release that holds name's change back: one written earlier in
// this pass, else the first of the cluster's with an operation in flight;
// "" when none. A Release that comes after name in the install order never
// holds it back.
func (g *helmGate) busy(ctx context.Context, name string) string {
	if g.started != "" && g.started != name && !g.comesAfter(g.started, name) {
		return g.started
	}
	for _, n := range g.names {
		if n != name && !g.comesAfter(n, name) && g.inFlight(ctx, n, g.live[n]) {
			return n
		}
	}
	return ""
}

// notRolledOut says what a new Release (name, not created yet) waits for:
// the first Release it is installed after that is not Ready (rollout) —
// a chart before it of its own component (the CRD chart, the main chart
// before a companion), else one of a component its component requires.
// "" when there is none. A required Release that does not exist is not
// waited for here: the plan's own conditions hold the new one until it
// is deployed.
func (g *helmGate) notRolledOut(ctx context.Context, name string) string {
	me, ok := g.items[name]
	if !ok {
		return ""
	}
	var before []string
	for _, n := range g.order[me.component] {
		if n == name {
			break
		}
		before = append(before, n)
	}
	for _, c := range g.requires[me.component] {
		before = append(before, g.order[c]...)
	}
	for _, n := range before {
		d := g.live[n]
		if d == nil {
			continue
		}
		if r := g.rollout(ctx, n, d); !r.Ready {
			chart := d.Chart
			if chart == "" {
				chart = n
			}
			return fmt.Sprintf("chart %s to roll out (%s)", chart, r.Message)
		}
	}
	return ""
}

// inFlight reports whether a Release has an operation running: not settled
// (Helm is installing it, or its workloads are rolling out), and not one
// of the states that run nothing: going away, paused by anyone (an
// operator's pause or the platform's backoff), given up, stuck past the
// time the operation may take (stuck: its recovery is no operation of the
// Release), or a rollout past its install timeout (it failed). A Release
// that failed and is retried still counts until its backoff pauses it.
func (g *helmGate) inFlight(ctx context.Context, name string, d *releaseDetail) bool {
	switch {
	case d.PausedByUs, stuck(d, now()), g.settled(ctx, name, d):
		return false
	}
	return !g.rollout(ctx, name, d).TimedOut
}

// firstToChange is the Release that must change before name's: of the
// Releases the pass's plans render that are not at the digest they must
// have, the first in the install order that comes before name's (a lower
// sequence, or one name's component requires). "" when name is first. A
// Release whose component cannot be written now (its values wait) is not
// waited for: it could not go first.
func (g *helmGate) firstToChange(name string) string {
	me, ok := g.items[name]
	if !ok {
		return ""
	}
	first := ""
	for _, n := range g.plannedNames() {
		it := g.items[n]
		if n == name || it.waits || it.component == me.component || !g.mustChange(n) {
			continue
		}
		if it.rank < me.rank || g.requiresComponent(me.component, it.component) {
			if first == "" || it.rank < g.items[first].rank {
				first = n
			}
		}
	}
	return first
}

// plannedNames are the planned Releases' names, sorted.
func (g *helmGate) plannedNames() []string {
	out := make([]string, 0, len(g.items))
	for n := range g.items {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// mustChange reports whether the planned Release has to be written: it does
// not exist, or its digest is not the one planned.
func (g *helmGate) mustChange(name string) bool {
	live := g.live[name]
	return live == nil || live.Digest != g.items[name].want
}

// comesAfter reports whether the live Release other is later in the
// install order than name: a higher sequence, or its component requires
// name's. A Release no plan renders (unknown) is never after.
func (g *helmGate) comesAfter(other, name string) bool {
	o, ok1 := g.items[other]
	n, ok2 := g.items[name]
	if !ok1 || !ok2 || o.component == n.component {
		return false
	}
	return o.rank > n.rank || g.requiresComponent(o.component, n.component)
}

// requiresComponent reports whether component a requires b, directly or
// through others.
func (g *helmGate) requiresComponent(a, b string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(c string) bool {
		for _, r := range g.requires[c] {
			if r == b {
				return true
			}
			if !seen[r] {
				seen[r] = true
				if walk(r) {
					return true
				}
			}
		}
		return false
	}
	return walk(a)
}

func (g *helmGate) holdMessage(busy string) string {
	chart := busy
	if d := g.live[busy]; d != nil && d.Chart != "" {
		chart = d.Chart
	}
	return fmt.Sprintf("Helm release %s to settle (one Helm operation per cluster at a time)", chart)
}

// releasesOf are the object names of the cluster's live Releases of the
// component (by its label), sorted; none when they cannot be read.
func (g *helmGate) releasesOf(ctx context.Context, component string) []string {
	g.load(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, n := range g.names {
		if g.live[n].component == component {
			out = append(out, n)
		}
	}
	return out
}

// readable reports whether the cluster's Releases could be read this pass.
func (g *helmGate) readable(ctx context.Context) bool {
	g.load(ctx)
	return g.err == nil
}

// settledAt reports whether every Release of want (object name to the
// digest it must have) is live at that digest and Ready (rollout: Helm
// deployed it, Synced, the provider caught up with its spec, and its
// workloads are available), not paused, given up or going away.
func (g *helmGate) settledAt(ctx context.Context, want map[string]string) bool {
	g.load(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil || len(want) == 0 {
		return false
	}
	for name, digest := range want {
		d := g.live[name]
		if d == nil || d.Digest != digest || d.deleting || d.behind || d.Paused || d.PausedByUs || d.GaveUp ||
			!d.Synced || !g.rollout(ctx, name, d).Ready {
			return false
		}
	}
	return true
}

// wasUnchanged reports whether the Release already had the digest the plan
// renders for it.
func (g *helmGate) wasUnchanged(object string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.unchanged[object]
}

// markUnchanged sets pc.Unchanged: every Release of the component already
// had the digest the pass rendered, so nothing was written for it.
func (g *helmGate) markUnchanged(pc *cloudv1.PlatformComponent, plan crossplaneaws.PlatformPlan, c crossplaneaws.PlatformComponentPlan) {
	objects := plan.ReleaseObjectNames(c)
	pc.Unchanged = len(objects) > 0 && !slices.ContainsFunc(objects, func(o string) bool { return !g.wasUnchanged(o) })
}
