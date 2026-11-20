package management

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// operationTimeout bounds what Helm waits for in a recovery: a rollback or
// uninstall that is not done by then is reported, and tried again by a
// later pass. The platform never waits for the rollout of what it recovers
// (provider-helm does that).
const operationTimeout = 2 * time.Minute

// helmOps is a workload cluster's Helm, the SDK's operations on the
// releases provider-helm manages there (reconcile.HelmOperator). The
// release records are read and written through the Helm storage the SDK
// uses (Secrets), as `helm` does.
type helmOps struct {
	helmStorage
	// configFor is Helm's configuration for a namespace; a test makes Helm
	// run on records in memory.
	configFor func(namespace string) (*action.Configuration, error)
}

// restGetter is a rest.Config as the SDK's RESTClientGetter, so the
// kubeconfig (already checked for credentials that run commands) is never
// written anywhere.
type restGetter struct {
	cfg       *rest.Config
	namespace string
}

func (g restGetter) ToRESTConfig() (*rest.Config, error) { return rest.CopyConfig(g.cfg), nil }

func (g restGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(g.cfg)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (g restGetter) ToRESTMapper() (apimeta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}

func (g restGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(*clientcmdapi.NewConfig(), &clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: g.namespace}})
}

// newHelmOps opens Helm on the cluster cfg and c talk to.
func newHelmOps(c client.Client, cfg *rest.Config) helmOps {
	return helmOps{helmStorage: helmStorage{c: c}, configFor: func(namespace string) (*action.Configuration, error) {
		out := &action.Configuration{}
		if err := out.Init(restGetter{cfg: cfg, namespace: namespace}, namespace, "secret", func(string, ...any) {}); err != nil {
			return nil, fmt.Errorf("open Helm on the workload cluster: %w", err)
		}
		return out, nil
	}}
}

func (h helmOps) config(namespace string) (*action.Configuration, error) {
	if h.configFor == nil {
		return nil, errors.New("helm is not available for this workload cluster")
	}
	return h.configFor(namespace)
}

// RollBack marks the stuck revision failed (Helm refuses to start an
// operation on a release whose newest revision is pending) and rolls the
// release back to revision `to`.
func (h helmOps) RollBack(ctx context.Context, namespace, name string, stuck, to int) error {
	cfg, err := h.config(namespace)
	if err != nil {
		return err
	}
	rel, err := cfg.Releases.Get(name, stuck)
	if err != nil {
		return fmt.Errorf("read revision %d of %s: %w", stuck, name, err)
	}
	if rel.Info.Status.IsPending() {
		rel.SetStatus(release.StatusFailed, "marked failed by cluster-vending: its operation never finished")
		if err := cfg.Releases.Update(rel); err != nil {
			return fmt.Errorf("mark revision %d of %s failed: %w", stuck, name, err)
		}
	}
	rollback := action.NewRollback(cfg)
	rollback.Version, rollback.Timeout, rollback.CleanupOnFail = to, operationTimeout, true
	if err := runBounded(ctx, func() error { return rollback.Run(name) }); err != nil {
		return err
	}
	return nil
}

// Uninstall removes the release and its history; a release that is not
// there is done.
func (h helmOps) Uninstall(ctx context.Context, namespace, name string) error {
	cfg, err := h.config(namespace)
	if err != nil {
		return err
	}
	uninstall := action.NewUninstall(cfg)
	uninstall.Timeout = operationTimeout
	err = runBounded(ctx, func() error { _, err := uninstall.Run(name); return err })
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	return err
}

// runBounded runs a Helm action, which takes no context, and gives up
// waiting for it when ctx ends.
func runBounded(ctx context.Context, f func() error) error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DeleteFailedHookJobs deletes the Jobs of release's hooks that failed in
// namespace (failedHookJob), in the background so their pods go too.
func (h helmOps) DeleteFailedHookJobs(ctx context.Context, namespace, name string) ([]string, error) {
	jobs := &batchv1.JobList{}
	if err := h.c.List(ctx, jobs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list the Jobs of %s: %w", namespace, err)
	}
	var deleted []string
	var errs []error
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !failedHookJob(job, name) {
			continue
		}
		if err := client.IgnoreNotFound(h.c.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
			errs = append(errs, fmt.Errorf("delete Job %s: %w", job.Name, err))
			continue
		}
		deleted = append(deleted, job.Name)
	}
	slices.Sort(deleted)
	return deleted, errors.Join(errs...)
}

// failedHookJob reports whether job is a Job one of release's hooks made
// and that failed for good: it carries Helm's hook annotation, belongs to
// the release (Helm's release-name annotation or the instance label; a
// hook without either is the release's when named after it), and has the
// Failed condition. A Job still retrying or running is left alone.
func failedHookJob(job *batchv1.Job, release string) bool {
	if job.Annotations["helm.sh/hook"] == "" {
		return false
	}
	owner := job.Annotations["meta.helm.sh/release-name"]
	if owner == "" {
		owner = job.Labels["app.kubernetes.io/instance"]
	}
	switch {
	case owner != "" && owner != release:
		return false
	case owner == "" && !strings.HasPrefix(job.Name, release+"-"):
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
