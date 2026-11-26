package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/helmrel"
)

// readiness is whether a component's Helm Release is Ready: the one notion
// of "installed" every part of a pass reads (rollout). The component's
// phase (READY), and through it the release move, the upgrade steps, the
// rollout gates, the catalog change, the API's and the console's status
// and the "is installed" event; the Helm gate's install order and its one
// operation per cluster; the backoff.
type readiness struct {
	// Ready: Helm deployed the chart and its workloads are available.
	Ready bool
	// Failed: Helm reports an error, or the rollout timed out (TimedOut).
	Failed, TimedOut bool
	// Message is what the Release waits for ("deployment/x: 1/3
	// available") or, when Failed, the error.
	Message string
}

// The bounds of reading a cluster's workloads in one pass.
const (
	// rolloutReadBudget is how long a pass may spend reading one
	// cluster's workloads, from the first read: an unreachable cluster
	// costs a pass this much at most.
	rolloutReadBudget = 30 * time.Second
	// workloadPageSize and maxWorkloadPages bound one list of a kind in a
	// namespace: a namespace with more is not read (its components are
	// not Ready, and say why).
	workloadPageSize = 250
	maxWorkloadPages = 4
)

// helmDeployed reports whether Helm deployed the Release's current spec:
// provider-helm acted on it, reports it Ready and Helm's state is
// deployed, with no error (Synced) unless it is paused, which stops the
// provider, not what runs.
func (d *releaseDetail) helmDeployed() bool {
	return !d.deleting && !d.behind && d.Ready && d.HelmState == crossplaneaws.HelmDeployed && (d.Synced || d.Paused || d.PausedByUs)
}

// installTimeout is how long the Release's workloads may take to become
// available after Helm deployed it.
func (d *releaseDetail) installTimeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return crossplaneaws.DefaultHelmTimeout
}

// rollout is the readiness of the Release object (d: its detail as read
// now). Ready once Helm deployed it (helmDeployed) and the Deployments,
// StatefulSets and DaemonSets of its release in its target namespace are
// available (helmrel.Unavailable); a chart with none is Ready once
// deployed. The install timeout runs from when the platform first saw
// Helm deploy the spec (the Release's helmrel.RolloutAnnotation): past it
// the rollout failed (TimedOut), which the backoff counts like the failed
// Helm wait it replaces. Once a revision was Ready it stays Ready (the
// annotation says so) without its workloads being read again: readiness
// is about a rollout finishing, not about every later pod.
//
// Without workload clients (Deps.Rollouts nil) a Release is Ready once
// Helm deployed it. One pass reads each namespace of a cluster once.
func (g *helmGate) rollout(ctx context.Context, object string, d *releaseDetail) readiness {
	switch {
	case d == nil:
		return readiness{Message: "its Helm Release to be created"}
	case g.wasWritten(object):
		// Its state is read from the next pass on: what provider-helm
		// reports now is about the spec before.
		return readiness{Message: fmt.Sprintf("provider-helm to apply the new spec of chart %s", d.Chart)}
	case !d.helmDeployed():
		return helmPending(d)
	}
	mark, marked := helmrel.ParseRolloutMark(d.rollout)
	marked = marked && mark.About(d.Digest, d.Revision)
	if marked && mark.Done {
		return readiness{Ready: true}
	}
	reader := g.workloadReader()
	if reader == nil {
		return readiness{Ready: true}
	}
	key := fmt.Sprintf("%s/%s/%d", object, d.Digest, d.Revision)
	g.rmu.Lock()
	if r, ok := g.rollouts[key]; ok {
		g.rmu.Unlock()
		return r
	}
	g.rmu.Unlock()

	at := now()
	r := readiness{Ready: true}
	problem, err := reader.unavailable(ctx, d.TargetNamespace, d.Chart)
	what, _, _ := strings.Cut(problem, ": ") // the workload, as a stable name
	switch {
	case err != nil:
		what = fmt.Sprintf("the workloads of chart %s to be read", d.Chart)
		problem = fmt.Sprintf("%s (%v)", what, err)
	case problem == "":
		g.markRollout(ctx, object, helmrel.RolloutMark{Digest: d.Digest, Revision: d.Revision, Done: true})
	}
	if problem != "" {
		since := mark.Since
		if !marked || since.IsZero() {
			since = at
			g.markRollout(ctx, object, helmrel.RolloutMark{Digest: d.Digest, Revision: d.Revision, Since: since})
		}
		r = readiness{Message: problem}
		if timeout := d.installTimeout(); at.Sub(since) >= timeout {
			r = readiness{Failed: true, TimedOut: true, Message: helmrel.RolloutTimedOut(timeout, what)}
		}
	}
	g.rmu.Lock()
	g.rollouts[key] = r
	g.rmu.Unlock()
	return r
}

// wasWritten reports whether the pass let the Release object be written.
func (g *helmGate) wasWritten(object string) bool {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	return g.written[object]
}

// helmPending is the readiness of a Release Helm has not deployed (yet):
// failed with the provider's error, or waiting for Helm.
func helmPending(d *releaseDetail) readiness {
	switch {
	case !d.Synced && d.LastError != "" && !d.Paused && !d.PausedByUs && !d.deleting:
		return readiness{Failed: true, Message: d.LastError}
	case d.behind:
		return readiness{Message: fmt.Sprintf("provider-helm to act on the new spec of chart %s", d.Chart)}
	case d.HelmState != "" && d.HelmState != crossplaneaws.HelmDeployed:
		return readiness{Message: fmt.Sprintf("Helm to deploy chart %s (%s)", d.Chart, d.HelmState)}
	}
	return readiness{Message: fmt.Sprintf("Helm to deploy chart %s", d.Chart)}
}

// markRollout records m on the Release (helmrel.RolloutAnnotation), a merge
// patch of its metadata like the backoff's. Best effort: a mark that was
// not written is written by a later pass (a deadline then starts later).
func (g *helmGate) markRollout(ctx context.Context, object string, m helmrel.RolloutMark) {
	if g.s == nil || g.s.deps.Kube == nil {
		return
	}
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{helmrel.RolloutAnnotation: m.String()}}})
	if err != nil {
		return
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	u.SetNamespace(g.namespace)
	u.SetName(object)
	if err := client.IgnoreNotFound(g.s.deps.Kube.Patch(ctx, u, client.RawPatch(client.Merge.Type(), raw))); err != nil {
		g.s.logger().WarnContext(ctx, "helm rollout not recorded on the Release", "cluster_id", g.cluster, "release", object, "error", err)
	}
}

// workloadReader is the pass's reader of the cluster's workloads; nil
// without workload clients.
func (g *helmGate) workloadReader() *workloads {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	if g.rollouts == nil {
		g.rollouts = map[string]readiness{}
	}
	if g.workloads == nil && g.s != nil && g.s.deps.Rollouts != nil {
		clients, namespace, secret := g.s.deps.Rollouts, g.namespace, g.kubeconfig
		g.workloads = &workloads{open: func(ctx context.Context) (client.Reader, error) {
			return clients.Client(ctx, namespace, secret)
		}, byNamespace: map[string]namespaceWorkloads{}}
	}
	return g.workloads
}

// workloads reads a workload cluster's Deployments, StatefulSets and
// DaemonSets for one pass: the client is opened once, each namespace is
// listed once (a list per kind, paged), and all of it within
// rolloutReadBudget of the first read.
type workloads struct {
	open func(context.Context) (client.Reader, error)

	mu       sync.Mutex
	opened   bool
	c        client.Reader
	err      error
	deadline time.Time
	// byNamespace are the namespaces read, with what was found.
	byNamespace map[string]namespaceWorkloads
}

type namespaceWorkloads struct {
	w   helmrel.Workloads
	err error
}

// unavailable is helmrel.Unavailable for release in namespace.
func (r *workloads) unavailable(ctx context.Context, namespace, release string) (string, error) {
	w, err := r.of(ctx, namespace)
	if err != nil {
		return "", err
	}
	return helmrel.Unavailable(w, release, namespace), nil
}

// of are the namespace's workloads, read once per pass.
func (r *workloads) of(ctx context.Context, namespace string) (helmrel.Workloads, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if got, ok := r.byNamespace[namespace]; ok {
		return got.w, got.err
	}
	if !r.opened {
		r.opened, r.deadline = true, now().Add(rolloutReadBudget)
		r.c, r.err = r.open(ctx)
	}
	if r.err != nil {
		return helmrel.Workloads{}, r.err
	}
	ctx, cancel := context.WithDeadline(ctx, r.deadline)
	defer cancel()
	var got namespaceWorkloads
	got.w, got.err = readWorkloads(ctx, r.c, namespace)
	r.byNamespace[namespace] = got
	return got.w, got.err
}

// readWorkloads lists the namespace's Deployments, StatefulSets and
// DaemonSets.
func readWorkloads(ctx context.Context, c client.Reader, namespace string) (helmrel.Workloads, error) {
	var out helmrel.Workloads
	deployments := &appsv1.DeploymentList{}
	if err := listPaged(ctx, c, namespace, deployments, func() { out.Deployments = append(out.Deployments, deployments.Items...) }); err != nil {
		return out, fmt.Errorf("list deployments: %w", err)
	}
	statefulSets := &appsv1.StatefulSetList{}
	if err := listPaged(ctx, c, namespace, statefulSets, func() { out.StatefulSets = append(out.StatefulSets, statefulSets.Items...) }); err != nil {
		return out, fmt.Errorf("list statefulsets: %w", err)
	}
	daemonSets := &appsv1.DaemonSetList{}
	if err := listPaged(ctx, c, namespace, daemonSets, func() { out.DaemonSets = append(out.DaemonSets, daemonSets.Items...) }); err != nil {
		return out, fmt.Errorf("list daemonsets: %w", err)
	}
	return out, nil
}

// listPaged lists a kind in namespace, workloadPageSize at a time, calling
// collect after each page; more than maxWorkloadPages pages is an error.
func listPaged(ctx context.Context, c client.Reader, namespace string, list client.ObjectList, collect func()) error {
	token := ""
	for range maxWorkloadPages {
		opts := []client.ListOption{client.InNamespace(namespace), client.Limit(workloadPageSize)}
		if token != "" {
			opts = append(opts, client.Continue(token))
		}
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		collect()
		if token = list.GetContinue(); token == "" {
			return nil
		}
	}
	return fmt.Errorf("more than %d in namespace %s", workloadPageSize*maxWorkloadPages, namespace)
}
