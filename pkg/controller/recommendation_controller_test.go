package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/recommend"
	"github.com/yindia/cluster-vending/pkg/reconcile/recommendations"
)

// usageCluster is a workload cluster with one Deployment and metrics.
type usageCluster struct{}

func (usageCluster) Workloads(context.Context) ([]recommend.Workload, error) {
	return []recommend.Workload{{Namespace: "web", Kind: "Deployment", Name: "site", Replicas: 1,
		Containers: []recommend.Container{{Name: "app", Requests: recommend.Resources{CPUMillis: 500}}}}}, nil
}
func (usageCluster) Pods(context.Context) ([]recommend.Pod, error) {
	return []recommend.Pod{{Namespace: "web", Name: "site-1-a", OwnerKind: "ReplicaSet", OwnerName: "site-1"}}, nil
}
func (usageCluster) PodMetrics(context.Context) ([]recommend.PodUsage, bool, error) {
	return []recommend.PodUsage{{Namespace: "web", Pod: "site-1-a",
		Containers: map[string]recommend.Resources{"app": {CPUMillis: 40, MemoryBytes: 64 << 20}}}}, true, nil
}
func (usageCluster) VPAs(context.Context) ([]recommend.VPA, bool, error) { return nil, false, nil }

func recommendationWorld(t *testing.T) (world, *RecommendationReconciler, *int) {
	t.Helper()
	w, _, _ := scanWorld(t) // a running cluster "prod" with its kubeconfig Secret
	r := NewRecommendationReconciler(w.deps)
	connected := 0
	r.sampler.Connect = func([]byte) (recommend.Reader, error) {
		connected++
		return usageCluster{}, nil
	}
	return w, r, &connected
}

func TestRunningClusterIsSampled(t *testing.T) {
	w, r, connected := recommendationWorld(t)

	res, err := r.Reconcile(t.Context(), request("prod"))

	if err != nil || res.RequeueAfter != recommendations.Interval || *connected != 1 {
		t.Fatalf("Reconcile = %+v, %v, %d connections", res, err, *connected)
	}
	rec := w.cluster(t, "prod").Status.Recommendations
	if rec == nil || rec.Containers != 1 || !rec.MetricsAvailable || rec.SampledAt == nil || rec.Error != "" {
		t.Fatalf("status.recommendations = %+v", rec)
	}
	cm := &corev1.ConfigMap{}
	w.must(t, w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "prod-usage-history"}, cm))
	h, err := recommend.Decode(cm.BinaryData["history.json.gz"])
	if err != nil || h.Containers() != 1 || h.Workloads[0].Containers[0].Samples != 1 {
		t.Fatalf("recorded usage = %+v, %v", h, err)
	}

	// Again right away: the sample stands, the workload cluster is not read.
	res, err = r.Reconcile(t.Context(), request("prod"))
	if err != nil || res.RequeueAfter <= 0 || res.RequeueAfter > recommendations.Interval || *connected != 1 {
		t.Fatalf("second Reconcile = %+v, %v, %d connections", res, err, *connected)
	}
}

func TestClusterThatIsNotSampled(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, world)
	}{
		{"paused by an operator", func(t *testing.T, w world) {
			w.pauseTenant(t, operatorPause)
		}},
		{"not running yet", func(t *testing.T, w world) {
			obj := w.cluster(t, "prod")
			obj.Status.Phase = "Provisioning"
			w.must(t, w.k.Status().Update(t.Context(), obj))
		}},
		{"switched off", func(*testing.T, world) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, r, connected := recommendationWorld(t)
			tt.change(t, w)
			if tt.name == "switched off" {
				r.enabled = false
			}

			res, err := r.Reconcile(t.Context(), request("prod"))

			if err != nil || *connected != 0 || w.cluster(t, "prod").Status.Recommendations != nil {
				t.Fatalf("Reconcile = %+v, %v; %d connections, status %+v", res, err, *connected, w.cluster(t, "prod").Status.Recommendations)
			}
		})
	}
}

// Namespaces of the catalog's components are the platform's.
func TestPlatformNamespacesComeFromTheCatalog(t *testing.T) {
	w, r, _ := recommendationWorld(t)
	w.must(t, w.k.Create(t.Context(), &cloudv1alpha1.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: "kyverno"},
		Spec: cloudv1alpha1.ComponentDefinitionSpec{Namespace: "kyverno"}}))

	platform := r.platformNamespaces(t.Context())

	if !platform("kyverno") || !platform("kube-system") || platform("web") {
		t.Errorf("kyverno %v, kube-system %v, web %v", platform("kyverno"), platform("kube-system"), platform("web"))
	}
}
