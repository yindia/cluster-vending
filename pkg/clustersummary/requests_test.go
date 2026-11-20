package clustersummary

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func rl(cpu, mem string) corev1.ResourceList {
	out := corev1.ResourceList{}
	if cpu != "" {
		out[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		out[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return out
}

func ctr(cpu, mem string) corev1.Container {
	return corev1.Container{Resources: corev1.ResourceRequirements{Requests: rl(cpu, mem)}}
}

func sidecar(cpu, mem string) corev1.Container {
	c := ctr(cpu, mem)
	c.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
	return c
}

func TestPodRequests(t *testing.T) {
	const mi = 1 << 20
	tests := []struct {
		name        string
		spec        corev1.PodSpec
		cpu, mem    int64
		withoutReqs bool
	}{
		{"no containers", corev1.PodSpec{}, 0, 0, false},
		{"sum of containers", corev1.PodSpec{Containers: []corev1.Container{ctr("100m", "64Mi"), ctr("250m", "128Mi")}}, 350, 192 * mi, false},
		{"init larger than sum", corev1.PodSpec{
			InitContainers: []corev1.Container{ctr("1", "1Gi")},
			Containers:     []corev1.Container{ctr("100m", "64Mi")}}, 1000, 1 << 30, false},
		{"init smaller than sum", corev1.PodSpec{
			InitContainers: []corev1.Container{ctr("50m", "10Mi")},
			Containers:     []corev1.Container{ctr("100m", "64Mi")}}, 100, 64 * mi, false},
		{"max of init containers, not their sum", corev1.PodSpec{
			InitContainers: []corev1.Container{ctr("300m", "10Mi"), ctr("200m", "20Mi")},
			Containers:     []corev1.Container{ctr("100m", "1Mi")}}, 300, 20 * mi, false},
		{"overhead added", corev1.PodSpec{Containers: []corev1.Container{ctr("100m", "64Mi")}, Overhead: rl("50m", "16Mi")}, 150, 80 * mi, false},
		{"overhead added to init peak", corev1.PodSpec{
			InitContainers: []corev1.Container{ctr("1", "")},
			Containers:     []corev1.Container{ctr("100m", "")}, Overhead: rl("50m", "")}, 1050, 0, true},
		{"missing requests are zero and flagged", corev1.PodSpec{Containers: []corev1.Container{ctr("100m", ""), ctr("", "")}}, 100, 0, true},
		{"missing only memory is flagged", corev1.PodSpec{Containers: []corev1.Container{ctr("100m", "")}}, 100, 0, true},
		{"init container without requests is not flagged", corev1.PodSpec{
			InitContainers: []corev1.Container{ctr("", "")},
			Containers:     []corev1.Container{ctr("100m", "64Mi")}}, 100, 64 * mi, false},
		{"sidecar adds to the running sum", corev1.PodSpec{
			InitContainers: []corev1.Container{sidecar("100m", "10Mi")},
			Containers:     []corev1.Container{ctr("200m", "20Mi")}}, 300, 30 * mi, false},
		{"init after sidecar peaks with the sidecar running", corev1.PodSpec{
			InitContainers: []corev1.Container{sidecar("100m", "10Mi"), ctr("1", "1Mi")},
			Containers:     []corev1.Container{ctr("200m", "20Mi")}}, 1100, 30 * mi, false},
		{"init before sidecar does not include it", corev1.PodSpec{
			InitContainers: []corev1.Container{ctr("1", "1Mi"), sidecar("100m", "10Mi")},
			Containers:     []corev1.Container{ctr("200m", "20Mi")}}, 1000, 30 * mi, false},
		{"fractional cpu rounds up to a millicore", corev1.PodSpec{Containers: []corev1.Container{ctr("1500u", "")}}, 2, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PodRequests(&corev1.Pod{Spec: tt.spec})
			if got.CPUMillis != tt.cpu || got.MemoryBytes != tt.mem || got.MissingRequests != tt.withoutReqs {
				t.Fatalf("got %+v, want cpu %d mem %d missing %v", got, tt.cpu, tt.mem, tt.withoutReqs)
			}
		})
	}
}

func TestCounted(t *testing.T) {
	tests := []struct {
		name  string
		node  string
		phase corev1.PodPhase
		want  bool
	}{
		{"running on a node", "n1", corev1.PodRunning, true},
		{"pending but bound", "n1", corev1.PodPending, true},
		{"unknown phase", "n1", "", true},
		{"succeeded", "n1", corev1.PodSucceeded, false},
		{"failed", "n1", corev1.PodFailed, false},
		{"not bound", "", corev1.PodPending, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &corev1.Pod{Spec: corev1.PodSpec{NodeName: tt.node}, Status: corev1.PodStatus{Phase: tt.phase}}
			if got := Counted(p); got != tt.want {
				t.Fatalf("Counted = %v, want %v", got, tt.want)
			}
		})
	}
}
