// Package clustersummary is the one definition of what the pods of a node request
// (the scheduler's), and of the per-group totals built from it. The
// controller's pass (the cluster status summary) and the live node build use
// it, so the two cannot disagree (PROP-02, 5.3).
package clustersummary

import (
	corev1 "k8s.io/api/core/v1"
)

// Requests is what one pod asks of its node.
type Requests struct {
	CPUMillis   int64
	MemoryBytes int64
	// MissingRequests: a container sets no CPU or no memory request.
	MissingRequests bool
}

// Counted reports whether the pod counts against its node: bound to a node
// and not finished (Succeeded or Failed).
func Counted(p *corev1.Pod) bool {
	if p.Spec.NodeName == "" {
		return false
	}
	return p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed
}

type pair struct{ cpu, mem int64 }

func (a pair) plus(b pair) pair { return pair{a.cpu + b.cpu, a.mem + b.mem} }

func (a pair) max(b pair) pair { return pair{max(a.cpu, b.cpu), max(a.mem, b.mem)} }

func requestOf(c *corev1.Container) pair {
	return pair{
		cpu: c.Resources.Requests.Cpu().MilliValue(),
		mem: c.Resources.Requests.Memory().Value(),
	}
}

func isSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// PodRequests is the scheduler's effective request of the pod: the larger of
// the containers' sum (plus the restartable init containers, which keep
// running) and the init containers' peak, plus spec.overhead, per resource.
// A missing request is zero. An init container runs alone (before the next
// one) unless it is a sidecar (restartPolicy Always), which then keeps running
// beside everything after it.
//
// A pod that sets pod-level spec.resources requests (Kubernetes 1.32 and
// later) counts those instead, per resource, as the scheduler does (PROP-02
// open question 1); a resource it sets there is never missing.
func PodRequests(p *corev1.Pod) Requests {
	var containers, sidecars, initPeak pair
	var missingCPU, missingMem bool
	for i := range p.Spec.Containers {
		c := &p.Spec.Containers[i]
		containers = containers.plus(requestOf(c))
		if _, ok := c.Resources.Requests[corev1.ResourceCPU]; !ok {
			missingCPU = true
		}
		if _, ok := c.Resources.Requests[corev1.ResourceMemory]; !ok {
			missingMem = true
		}
	}
	for i := range p.Spec.InitContainers {
		c := &p.Spec.InitContainers[i]
		req := requestOf(c)
		if isSidecar(c) {
			sidecars = sidecars.plus(req)
			initPeak = initPeak.max(sidecars)
			continue
		}
		initPeak = initPeak.max(sidecars.plus(req))
	}
	total := containers.plus(sidecars).max(initPeak)
	if pod := p.Spec.Resources; pod != nil {
		if q, ok := pod.Requests[corev1.ResourceCPU]; ok {
			total.cpu, missingCPU = q.MilliValue(), false
		}
		if q, ok := pod.Requests[corev1.ResourceMemory]; ok {
			total.mem, missingMem = q.Value(), false
		}
	}
	over := pair{p.Spec.Overhead.Cpu().MilliValue(), p.Spec.Overhead.Memory().Value()}
	total = total.plus(over)
	return Requests{CPUMillis: total.cpu, MemoryBytes: total.mem, MissingRequests: missingCPU || missingMem}
}
