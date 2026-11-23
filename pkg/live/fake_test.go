package live

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeCluster is a workload API that pages its lists (limit and continue),
// and can fail, expire a continue token, or hang.
type fakeCluster struct {
	mu    sync.Mutex
	items map[schema.GroupVersionResource][]map[string]any
	// errs fail a kind's lists; expire makes the first n continued calls
	// answer 410 Gone; hang blocks every call until the context ends;
	// hangAfter blocks only calls after the first.
	errs      map[schema.GroupVersionResource]error
	expire    int
	hang      bool
	hangAfter bool
	calls     int
	gate      chan struct{}
}

func newFake() *fakeCluster {
	return &fakeCluster{items: map[schema.GroupVersionResource][]map[string]any{}, errs: map[schema.GroupVersionResource]error{}}
}

func (f *fakeCluster) add(gvr schema.GroupVersionResource, objs ...map[string]any) *fakeCluster {
	f.items[gvr] = append(f.items[gvr], objs...)
	return f
}

func (f *fakeCluster) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCluster) List(ctx context.Context, gvr schema.GroupVersionResource, limit int64, cont string) (*unstructured.UnstructuredList, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	gate := f.gate
	hang := f.hang || f.hangAfter && n > 1
	expire := cont != "" && f.expire > 0
	if expire {
		f.expire--
	}
	err := f.errs[gvr]
	items := f.items[gvr]
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if expire {
		return nil, apierrors.NewResourceExpired("the continue token expired")
	}
	if err != nil {
		return nil, err
	}
	start := 0
	if cont != "" {
		start, _ = strconv.Atoi(cont)
	}
	end := min(start+int(limit), len(items))
	out := &unstructured.UnstructuredList{}
	for _, o := range items[start:end] {
		out.Items = append(out.Items, unstructured.Unstructured{Object: o})
	}
	if end < len(items) {
		out.SetContinue(strconv.Itoa(end))
	}
	return out, nil
}

// fakeOpener opens one fake cluster per "<tenant>/<cluster>"; err fails Open.
type fakeOpener struct {
	clusters map[string]*fakeCluster
	err      error
	opens    int
	mu       sync.Mutex
}

func (o *fakeOpener) Open(_ context.Context, tenant, cluster string) (Lister, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opens++
	if o.err != nil {
		return nil, o.err
	}
	c, ok := o.clusters[tenant+"/"+cluster]
	if !ok {
		return nil, newError(ReasonUnreachable, "no cluster")
	}
	return c, nil
}

// clock is a settable time.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func node(name, group string, ready bool, cpu, mem string, created time.Time) map[string]any {
	labels := map[string]any{"node.kubernetes.io/instance-type": "m7g.large"}
	if group == SystemGroup {
		labels[labelManagedPool] = "prod-default"
	} else if group != "" {
		labels[labelNodePool] = group
	}
	status := "False"
	if ready {
		status = "True"
	}
	return map[string]any{"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": name, "labels": labels, "creationTimestamp": created.UTC().Format(time.RFC3339)},
		"status": map[string]any{"allocatable": map[string]any{"cpu": cpu, "memory": mem, "pods": "58"},
			"conditions": []any{map[string]any{"type": "Ready", "status": status}}}}
}

type ctr struct{ cpu, mem string }

func pod(name, nodeName, phase string, init []ctr, containers ...ctr) map[string]any {
	list := func(cs []ctr) []any {
		out := []any{}
		for i, c := range cs {
			req := map[string]any{}
			if c.cpu != "" {
				req["cpu"] = c.cpu
			}
			if c.mem != "" {
				req["memory"] = c.mem
			}
			out = append(out, map[string]any{"name": "c" + strconv.Itoa(i), "resources": map[string]any{"requests": req}})
		}
		return out
	}
	return map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"spec":     map[string]any{"nodeName": nodeName, "containers": list(containers), "initContainers": list(init)},
		"status":   map[string]any{"phase": phase}}
}

func nodeMetrics(name, cpu, mem string) map[string]any {
	return map[string]any{"apiVersion": "metrics.k8s.io/v1beta1", "kind": "NodeMetrics",
		"metadata": map[string]any{"name": name}, "usage": map[string]any{"cpu": cpu, "memory": mem}}
}

var errRefused = errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
