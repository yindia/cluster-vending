package reconcile

import (
	"context"
	"time"
)

// TeardownListLimit bounds how many objects of one kind a Teardown reads in
// a pass: one page. A pass deletes what it saw and the next sees the rest,
// so a workload cluster with thousands of NodeClaims never costs a pass an
// unbounded read.
const TeardownListLimit = 500

// DefaultTeardownTimeout bounds a cluster deletion's teardown (Deps):
// deleting a load balancer takes a minute or two, draining and
// terminating Karpenter's nodes a few minutes more.
const DefaultTeardownTimeout = 15 * time.Minute

// Kinds of the workload-cluster objects whose controllers made cloud
// resources a cluster's deletion must not leave behind: load balancers
// (and their security groups) of the AWS Load Balancer Controller, and
// Karpenter's EC2 instances, launch templates and instance profiles.
const (
	KindIngress      = "Ingress"
	KindService      = "Service"
	KindNodePool     = "NodePool"
	KindNodeClaim    = "NodeClaim"
	KindEC2NodeClass = "EC2NodeClass"
)

// WorkloadObject is one such object in a workload cluster.
type WorkloadObject struct {
	Kind, Namespace, Name string
	// Deleting: its deletion was asked for (it waits for its controller
	// to remove what it made in the cloud).
	Deleting bool
	// DeletedAt is when its deletion was asked for (its deletion
	// timestamp); zero when it is not being deleted.
	DeletedAt time.Time
}

// Ref is the object as a person reads it: "Service istio-system/gw".
func (o WorkloadObject) Ref() string {
	if o.Namespace == "" {
		return o.Kind + " " + o.Name
	}
	return o.Kind + " " + o.Namespace + "/" + o.Name
}

// Teardown is a workload cluster's objects of those kinds: what a cluster
// deletion removes before it uninstalls the controllers that would clean
// up after them.
type Teardown interface {
	// List is the objects of the given kinds (Kind* above): every Ingress
	// of the AWS Load Balancer Controller (class alb), Service of type
	// LoadBalancer, and Karpenter NodePool, NodeClaim and EC2NodeClass,
	// at most TeardownListLimit of a kind per call; a kind the cluster
	// does not serve is left out.
	List(ctx context.Context, kinds ...string) ([]WorkloadObject, error)
	// Delete asks for one object's deletion; gone already is no error.
	Delete(ctx context.Context, o WorkloadObject) error
}

// Teardowns opens the Teardown of the workload cluster whose kubeconfig
// Secret is secretRef (in namespace, as Workloads.HelmStorage).
type Teardowns interface {
	Open(ctx context.Context, namespace, secretRef string) (Teardown, error)
}
