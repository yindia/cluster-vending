package recommend

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// pageSize is how many objects one list call asks for.
	pageSize = 250
	// maxObjects bounds what is read of one kind: a cluster with more is
	// sampled up to here.
	maxObjects = 20000
)

var (
	podMetricsGVK = schema.GroupVersionKind{Group: "metrics.k8s.io", Version: "v1beta1", Kind: "PodMetrics"}
	vpaGVK        = schema.GroupVersionKind{Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscaler"}
	podGVK        = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
	// workloadKinds are the kinds whose pod templates are read.
	workloadKinds = []string{"Deployment", "StatefulSet", "DaemonSet"}
)

// KubeReader reads a workload cluster through one client: the workloads,
// the metrics API and the VPAs as unstructured objects, and the pods as
// metadata only. It needs no Go types of the metrics or VPA APIs, and it
// only lists.
type KubeReader struct {
	Objects client.Reader
}

// NewKubeReader is a KubeReader on cfg's cluster. cfg must already be
// safe to use (deprecation.RESTConfig refuses exec plugins and auth
// providers).
func NewKubeReader(cfg *rest.Config) (*KubeReader, error) {
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, err
	}
	return &KubeReader{Objects: c}, nil
}

// list reads every object of gvk, page by page, up to maxObjects.
func (k *KubeReader) list(ctx context.Context, gvk schema.GroupVersionKind, each func(map[string]any)) error {
	token := ""
	for read := 0; read < maxObjects; {
		page := &unstructured.UnstructuredList{}
		page.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := k.Objects.List(ctx, page, &client.ListOptions{Limit: pageSize, Continue: token}); err != nil {
			return err
		}
		for i := range page.Items {
			each(page.Items[i].Object)
		}
		read += len(page.Items)
		if token = page.GetContinue(); token == "" {
			break
		}
	}
	return nil
}

// Workloads lists the Deployments, StatefulSets and DaemonSets.
func (k *KubeReader) Workloads(ctx context.Context) ([]Workload, error) {
	var out []Workload
	for _, kind := range workloadKinds {
		err := k.list(ctx, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind}, func(obj map[string]any) {
			out = append(out, WorkloadOf(kind, obj))
		})
		if err != nil {
			return nil, fmt.Errorf("%ss: %w", kind, err)
		}
	}
	return out, nil
}

// Pods lists the pods' metadata: their names and owners.
func (k *KubeReader) Pods(ctx context.Context) ([]Pod, error) {
	var out []Pod
	token := ""
	for len(out) < maxObjects {
		page := &metav1.PartialObjectMetadataList{}
		page.SetGroupVersionKind(podGVK.GroupVersion().WithKind("PodList"))
		if err := k.Objects.List(ctx, page, &client.ListOptions{Limit: pageSize, Continue: token}); err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			p := Pod{Namespace: item.Namespace, Name: item.Name}
			for _, ref := range item.OwnerReferences {
				if ref.Controller != nil && *ref.Controller {
					p.OwnerKind, p.OwnerName = ref.Kind, ref.Name
				}
			}
			out = append(out, p)
		}
		if token = page.Continue; token == "" {
			break
		}
	}
	return out, nil
}

// missing reports whether err says the cluster does not have the kind.
func missing(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}

// PodMetrics lists what the metrics API reports. A cluster without the
// API (no metrics-server) is not an error.
func (k *KubeReader) PodMetrics(ctx context.Context) ([]PodUsage, bool, error) {
	var out []PodUsage
	err := k.list(ctx, podMetricsGVK, func(obj map[string]any) { out = append(out, PodUsageOf(obj)) })
	if missing(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	return out, true, nil
}

// VPAs lists the Vertical Pod Autoscalers that recommend something. A
// cluster without the kind is not an error.
func (k *KubeReader) VPAs(ctx context.Context) ([]VPA, bool, error) {
	var out []VPA
	err := k.list(ctx, vpaGVK, func(obj map[string]any) {
		if v := VPAOf(obj); len(v.Containers) > 0 {
			out = append(out, v)
		}
	})
	if missing(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	return out, true, nil
}

// WorkloadOf reads a Deployment, StatefulSet or DaemonSet: its containers
// with the requests and limits of the pod template. A DaemonSet's
// replicas are the nodes it is scheduled on.
func WorkloadOf(kind string, obj map[string]any) Workload {
	w := Workload{Kind: kind}
	w.Namespace, _, _ = unstructured.NestedString(obj, "metadata", "namespace")
	w.Name, _, _ = unstructured.NestedString(obj, "metadata", "name")
	replicas, found := integerAt(obj, "spec", "replicas")
	if kind == "DaemonSet" {
		replicas, found = integerAt(obj, "status", "desiredNumberScheduled")
	}
	if !found && kind != "DaemonSet" {
		replicas = 1 // the API's default
	}
	w.Replicas = int32(max(min(replicas, 1<<31-1), 0))
	containers, _, _ := unstructured.NestedSlice(obj, "spec", "template", "spec", "containers")
	for _, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "name")
		w.Containers = append(w.Containers, Container{Name: name,
			Requests: resourcesAt(m, "resources", "requests"), Limits: resourcesAt(m, "resources", "limits")})
	}
	return w
}

// PodUsageOf reads a PodMetrics object.
func PodUsageOf(obj map[string]any) PodUsage {
	u := PodUsage{Containers: map[string]Resources{}}
	u.Namespace, _, _ = unstructured.NestedString(obj, "metadata", "namespace")
	u.Pod, _, _ = unstructured.NestedString(obj, "metadata", "name")
	containers, _, _ := unstructured.NestedSlice(obj, "containers")
	for _, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "name")
		u.Containers[name] = resourcesAt(m, "usage")
	}
	return u
}

// VPAOf reads a VerticalPodAutoscaler: its target and what its status
// recommends per container.
func VPAOf(obj map[string]any) VPA {
	v := VPA{Containers: map[string]VPARecommendation{}}
	v.Namespace, _, _ = unstructured.NestedString(obj, "metadata", "namespace")
	v.TargetKind, _, _ = unstructured.NestedString(obj, "spec", "targetRef", "kind")
	v.TargetName, _, _ = unstructured.NestedString(obj, "spec", "targetRef", "name")
	recs, _, _ := unstructured.NestedSlice(obj, "status", "recommendation", "containerRecommendations")
	for _, r := range recs {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "containerName")
		rec := VPARecommendation{Target: resourcesAt(m, "target"), UpperBound: resourcesAt(m, "upperBound")}
		if rec.Target != (Resources{}) {
			v.Containers[name] = rec
		}
	}
	return v
}

// integerAt reads a whole number at path, however the decoder typed it.
func integerAt(obj map[string]any, path ...string) (int64, bool) {
	v, found, _ := unstructured.NestedFieldNoCopy(obj, path...)
	if !found {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	}
	return 0, false
}

// resourcesAt reads the cpu and memory quantities of the map at path; a
// quantity that is missing or cannot be parsed is zero.
func resourcesAt(obj map[string]any, path ...string) Resources {
	m, _, _ := unstructured.NestedMap(obj, path...)
	var out Resources
	if q, ok := quantity(m["cpu"]); ok {
		out.CPUMillis = q.MilliValue()
	}
	if q, ok := quantity(m["memory"]); ok {
		out.MemoryBytes = q.Value()
	}
	return out
}

func quantity(v any) (resource.Quantity, bool) {
	s, ok := v.(string)
	if !ok {
		return resource.Quantity{}, false
	}
	q, err := resource.ParseQuantity(s)
	return q, err == nil
}
