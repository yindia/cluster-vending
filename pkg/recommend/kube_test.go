package recommend_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/recommend"
)

func object(t *testing.T, doc string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const deployment = `
apiVersion: apps/v1
kind: Deployment
metadata: {namespace: shop, name: web}
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: app
        resources:
          requests: {cpu: 250m, memory: 512Mi}
          limits: {cpu: "1", memory: 1Gi}
      - name: sidecar
`

func TestWorkloadOf(t *testing.T) {
	tests := []struct {
		name, kind, doc string
		want            recommend.Workload
	}{
		{"a deployment with and without resources", "Deployment", deployment,
			recommend.Workload{Namespace: "shop", Kind: "Deployment", Name: "web", Replicas: 3, Containers: []recommend.Container{
				{Name: "app", Requests: recommend.Resources{CPUMillis: 250, MemoryBytes: 512 * mi},
					Limits: recommend.Resources{CPUMillis: 1000, MemoryBytes: gi}},
				{Name: "sidecar"}}}},
		{"replicas left to the default", "StatefulSet", `
metadata: {namespace: shop, name: db}
spec: {template: {spec: {containers: [{name: postgres, resources: {requests: {memory: 1G}}}]}}}
`, recommend.Workload{Namespace: "shop", Kind: "StatefulSet", Name: "db", Replicas: 1,
			Containers: []recommend.Container{{Name: "postgres", Requests: recommend.Resources{MemoryBytes: 1e9}}}}},
		{"a daemonset counts the nodes it runs on", "DaemonSet", `
metadata: {namespace: kube-system, name: agent}
spec: {template: {spec: {containers: [{name: agent, resources: {requests: {cpu: not-a-quantity}}}]}}}
status: {desiredNumberScheduled: 5}
`, recommend.Workload{Namespace: "kube-system", Kind: "DaemonSet", Name: "agent", Replicas: 5,
			Containers: []recommend.Container{{Name: "agent"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recommend.WorkloadOf(tt.kind, object(t, tt.doc))

			if got.Namespace != tt.want.Namespace || got.Kind != tt.want.Kind || got.Name != tt.want.Name ||
				got.Replicas != tt.want.Replicas || len(got.Containers) != len(tt.want.Containers) {
				t.Fatalf("workload = %+v, want %+v", got, tt.want)
			}
			for i, c := range got.Containers {
				if c != tt.want.Containers[i] {
					t.Errorf("container %d = %+v, want %+v", i, c, tt.want.Containers[i])
				}
			}
		})
	}
}

func TestPodUsageOf(t *testing.T) {
	got := recommend.PodUsageOf(object(t, `
metadata: {namespace: shop, name: web-7d9f-abc}
containers:
- {name: app, usage: {cpu: 12345678n, memory: 204800Ki}}
- {name: sidecar, usage: {cpu: 2m, memory: 10Mi}}
`))

	if got.Namespace != "shop" || got.Pod != "web-7d9f-abc" ||
		got.Containers["app"] != (recommend.Resources{CPUMillis: 13, MemoryBytes: 200 * mi}) ||
		got.Containers["sidecar"] != (recommend.Resources{CPUMillis: 2, MemoryBytes: 10 * mi}) {
		t.Fatalf("usage = %+v", got)
	}
}

func TestVPAOf(t *testing.T) {
	got := recommend.VPAOf(object(t, `
metadata: {namespace: shop, name: web}
spec: {targetRef: {apiVersion: apps/v1, kind: Deployment, name: web}}
status:
  recommendation:
    containerRecommendations:
    - containerName: app
      target: {cpu: 120m, memory: 262144k}
      upperBound: {cpu: "1", memory: 1Gi}
    - containerName: empty
`))

	want := recommend.VPARecommendation{Target: recommend.Resources{CPUMillis: 120, MemoryBytes: 262144000},
		UpperBound: recommend.Resources{CPUMillis: 1000, MemoryBytes: gi}}
	if got.TargetKind != "Deployment" || got.TargetName != "web" || len(got.Containers) != 1 || got.Containers["app"] != want {
		t.Fatalf("vpa = %+v", got)
	}
}

// workloadCluster is a fake API server with the apps kinds and pods, and
// with or without the metrics and VPA kinds.
func workloadCluster(t *testing.T, extras bool, objects ...client.Object) *recommend.KubeReader {
	t.Helper()
	scheme := runtime.NewScheme()
	kinds := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"}, {Group: "apps", Version: "v1", Kind: "StatefulSet"},
		{Group: "apps", Version: "v1", Kind: "DaemonSet"}, {Version: "v1", Kind: "Pod"},
	}
	if extras {
		kinds = append(kinds, schema.GroupVersionKind{Group: "metrics.k8s.io", Version: "v1beta1", Kind: "PodMetrics"},
			schema.GroupVersionKind{Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscaler"})
	}
	for _, gvk := range kinds {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	// A real API server answers "no such kind" for an API it does not
	// serve; the fake client would return an empty list.
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
	if !extras {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				gvk := list.GetObjectKind().GroupVersionKind()
				if gvk.Group == "metrics.k8s.io" || gvk.Group == "autoscaling.k8s.io" {
					return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
				}
				return c.List(ctx, list, opts...)
			}})
	}
	return &recommend.KubeReader{Objects: builder.Build()}
}

func unstructuredOf(t *testing.T, apiVersion, kind, doc string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{Object: object(t, doc)}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	return u
}

func TestKubeReaderReadsTheCluster(t *testing.T) {
	pod := unstructuredOf(t, "v1", "Pod", "metadata: {namespace: shop, name: web-7d9f-abc}")
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-7d9f", UID: "1", Controller: ptr.To(true)}})
	reader := workloadCluster(t, true,
		unstructuredOf(t, "apps/v1", "Deployment", deployment), pod,
		unstructuredOf(t, "metrics.k8s.io/v1beta1", "PodMetrics", `
metadata: {namespace: shop, name: web-7d9f-abc}
containers: [{name: app, usage: {cpu: 100m, memory: 300Mi}}]
`),
		unstructuredOf(t, "autoscaling.k8s.io/v1", "VerticalPodAutoscaler", `
metadata: {namespace: shop, name: web}
spec: {targetRef: {kind: Deployment, name: web}}
status: {recommendation: {containerRecommendations: [{containerName: app, target: {cpu: 150m, memory: 400Mi}}]}}
`))

	s, err := recommend.Sample(t.Context(), reader, start)

	if err != nil || !s.Metrics || !s.VPA || len(s.Workloads) != 1 {
		t.Fatalf("sample = %+v, %v", s, err)
	}
	if got := s.Usage[webKey]; got != (recommend.Resources{CPUMillis: 100, MemoryBytes: 300 * mi}) {
		t.Errorf("usage = %+v", s.Usage)
	}
	if got := s.VPARecommendations[webKey].Target; got != (recommend.Resources{CPUMillis: 150, MemoryBytes: 400 * mi}) {
		t.Errorf("vpa = %+v", s.VPARecommendations)
	}
}

// A cluster without metrics-server and without the VPA kind is sampled
// for its workloads alone.
func TestKubeReaderWithoutMetricsAndVPA(t *testing.T) {
	reader := workloadCluster(t, false, unstructuredOf(t, "apps/v1", "Deployment", deployment))

	s, err := recommend.Sample(t.Context(), reader, start)

	if err != nil || s.Metrics || s.VPA || len(s.Workloads) != 1 || len(s.Usage) != 0 || len(s.Notes) != 0 {
		t.Fatalf("sample = %+v, %v", s, err)
	}
}
