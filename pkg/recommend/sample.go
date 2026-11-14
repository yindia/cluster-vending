package recommend

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SampleTimeout bounds one sample of a cluster.
const SampleTimeout = 2 * time.Minute

// Pod is a pod with the object that owns it: a ReplicaSet (of a
// Deployment), a StatefulSet or a DaemonSet.
type Pod struct{ Namespace, Name, OwnerKind, OwnerName string }

// PodUsage is what the metrics API reports a pod's containers use now.
type PodUsage struct {
	Namespace, Pod string
	Containers     map[string]Resources
}

// VPA is a Vertical Pod Autoscaler with what it recommends per container
// of its target.
type VPA struct {
	Namespace, TargetKind, TargetName string
	Containers                        map[string]VPARecommendation
}

// Reader reads a workload cluster. Nothing is written.
type Reader interface {
	// Workloads are the Deployments, StatefulSets and DaemonSets.
	Workloads(ctx context.Context) ([]Workload, error)
	// Pods are the pods with their owners.
	Pods(ctx context.Context) ([]Pod, error)
	// PodMetrics is the usage the metrics API serves; available is false
	// when the cluster has no metrics API (no metrics-server).
	PodMetrics(ctx context.Context) (usage []PodUsage, available bool, err error)
	// VPAs are the Vertical Pod Autoscalers; available is false when the
	// cluster does not have the kind.
	VPAs(ctx context.Context) (vpas []VPA, available bool, err error)
}

// Snapshot is one sample of a cluster.
type Snapshot struct {
	At        time.Time
	Workloads []Workload
	// Usage is, per container of a workload, the most any of its pods
	// uses now. A container without a running pod has no entry.
	Usage map[Key]Resources
	// VPARecommendations are the cluster's own VPA recommendations.
	VPARecommendations map[Key]VPARecommendation
	// Metrics and VPA: the cluster has the metrics API and the VPA kind.
	Metrics, VPA bool
	// Notes say what could not be read (the metrics API is down): the
	// sample is still used for the rest.
	Notes []string
}

// Sample reads the cluster once. A cluster without the metrics API or the
// VPA kind is sampled without them; a metrics API or VPA list that fails
// is a note, not an error. Only the workloads themselves must be readable.
func Sample(ctx context.Context, r Reader, at time.Time) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, SampleTimeout)
	defer cancel()
	s := Snapshot{At: at, Usage: map[Key]Resources{}, VPARecommendations: map[Key]VPARecommendation{}}
	var err error
	if s.Workloads, err = r.Workloads(ctx); err != nil {
		return Snapshot{}, fmt.Errorf("list workloads: %w", err)
	}
	known := map[[3]string]bool{}
	for _, w := range s.Workloads {
		known[[3]string{w.Namespace, w.Kind, w.Name}] = true
	}

	usage, available, err := r.PodMetrics(ctx)
	s.Metrics = available
	switch {
	case err != nil:
		s.Notes = append(s.Notes, "the metrics API did not answer: "+err.Error())
	case available:
		pods, err := r.Pods(ctx)
		if err != nil {
			return Snapshot{}, fmt.Errorf("list pods: %w", err)
		}
		owners := map[[2]string][2]string{}
		for _, p := range pods {
			if kind, name, ok := workloadOf(p, known); ok {
				owners[[2]string{p.Namespace, p.Name}] = [2]string{kind, name}
			}
		}
		for _, u := range usage {
			owner, ok := owners[[2]string{u.Namespace, u.Pod}]
			if !ok {
				continue
			}
			for container, used := range u.Containers {
				key := Key{u.Namespace, owner[0], owner[1], container}
				have := s.Usage[key]
				s.Usage[key] = Resources{CPUMillis: max(have.CPUMillis, used.CPUMillis), MemoryBytes: max(have.MemoryBytes, used.MemoryBytes)}
			}
		}
	}

	vpas, available, err := r.VPAs(ctx)
	s.VPA = available
	if err != nil {
		s.Notes = append(s.Notes, "the Vertical Pod Autoscalers could not be listed: "+err.Error())
	}
	for _, v := range vpas {
		if !known[[3]string{v.Namespace, v.TargetKind, v.TargetName}] {
			continue
		}
		for container, rec := range v.Containers {
			s.VPARecommendations[Key{v.Namespace, v.TargetKind, v.TargetName, container}] = rec
		}
	}
	return s, nil
}

// workloadOf is the workload a pod belongs to. A Deployment's pods are
// owned by a ReplicaSet named "<deployment>-<hash>": the Deployment is
// found by that name, among the Deployments that exist, so no ReplicaSet
// is listed.
func workloadOf(p Pod, known map[[3]string]bool) (kind, name string, ok bool) {
	switch p.OwnerKind {
	case "StatefulSet", "DaemonSet":
		return p.OwnerKind, p.OwnerName, known[[3]string{p.Namespace, p.OwnerKind, p.OwnerName}]
	case "ReplicaSet":
		i := strings.LastIndex(p.OwnerName, "-")
		if i <= 0 {
			return "", "", false
		}
		name = p.OwnerName[:i]
		return "Deployment", name, known[[3]string{p.Namespace, "Deployment", name}]
	}
	return "", "", false
}
