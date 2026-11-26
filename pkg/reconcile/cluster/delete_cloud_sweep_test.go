package cluster

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/stretchr/testify/require"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// fakeAccount is the tenant account's EC2 as the sweep sees it: Karpenter
// instances by ID (tagged for a cluster), each terminating in the pass
// after it was asked to (never, when stuck). The sweep has no ELB client
// here; awscloud's tests cover the load balancers.
type fakeAccount struct {
	mu         sync.Mutex
	instances  map[string]string // id -> cluster it is tagged for
	terminated map[string]int    // id -> passes since it was asked to
	stuck      bool
	failOnce   error
	// terminateCalls are the instances asked to terminate, in order.
	terminateCalls []string
	regions        []string
}

func (f *fakeAccount) api(_ awscloud.Access, region string) (awscloud.SweepAPI, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regions = append(f.regions, region)
	return awscloud.SweepAPI{EC2: f}, nil
}

func (f *fakeAccount) live() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.instances {
		if f.terminated[id] < 2 {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeAccount) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOnce; err != nil {
		f.failOnce = nil
		return nil, err
	}
	var want string
	for _, flt := range in.Filters {
		if name := ptr.Deref(flt.Name, ""); strings.HasPrefix(name, "tag:"+awscloud.TagClusterPrefix) {
			want = strings.TrimPrefix(name, "tag:"+awscloud.TagClusterPrefix)
		}
	}
	res := ec2types.Reservation{}
	for id, cluster := range f.instances {
		if cluster != want {
			continue
		}
		state := ec2types.InstanceStateNameRunning
		if n, asked := f.terminated[id]; asked {
			if n >= 1 && !f.stuck {
				f.terminated[id] = 2
				continue // terminated
			}
			state = ec2types.InstanceStateNameShuttingDown
			f.terminated[id] = n + 1
		}
		res.Instances = append(res.Instances, ec2types.Instance{InstanceId: ptr.To(id), State: &ec2types.InstanceState{Name: state},
			Tags: []ec2types.Tag{{Key: ptr.To(awscloud.TagKarpenterNodePool), Value: ptr.To("default")},
				{Key: ptr.To(awscloud.TagClusterPrefix + cluster), Value: ptr.To("owned")}}})
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{res}}, nil
}

func (f *fakeAccount) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range in.InstanceIds {
		f.terminateCalls = append(f.terminateCalls, id)
		if _, asked := f.terminated[id]; !asked {
			f.terminated[id] = 0
		}
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeAccount) DescribeLaunchTemplates(context.Context, *ec2.DescribeLaunchTemplatesInput, ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error) {
	return &ec2.DescribeLaunchTemplatesOutput{}, nil
}

func (f *fakeAccount) DeleteLaunchTemplate(context.Context, *ec2.DeleteLaunchTemplateInput, ...func(*ec2.Options)) (*ec2.DeleteLaunchTemplateOutput, error) {
	return &ec2.DeleteLaunchTemplateOutput{}, nil
}

func (f *fakeAccount) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{}, nil
}

func (f *fakeAccount) DeleteSecurityGroup(context.Context, *ec2.DeleteSecurityGroupInput, ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

// withSweep is a deletion whose account has two Karpenter instances of the
// cluster and one of another cluster.
func withSweep(t *testing.T) (*delOrder, *fakeAccount) {
	t.Helper()
	d := newDelOrder(t)
	f := &fakeAccount{instances: map[string]string{"i-ours1": d.row.Name, "i-ours2": d.row.Name, "i-theirs": "someone-else"},
		terminated: map[string]int{}}
	d.svc.deps.CloudSweep = f.api
	return d, f
}

func (d *delOrder) nodeRoleDeleted() bool {
	return slices.ContainsFunc(d.log, func(l string) bool { return strings.HasSuffix(l, "Role "+d.prefix+"-karpenter-node") })
}

func eventsOf(row *model.Cluster, reason string) []string {
	var out []string
	for _, e := range storedStatus(row).GetRecentEvents() {
		if e.GetReason() == reason {
			out = append(out, e.GetMessage())
		}
	}
	return out
}

// The sweep terminates the cluster's Karpenter instances and waits until
// they are gone before Karpenter's node role is deleted; another cluster's
// instance is never touched; the status records that it finished.
func TestDeletionSweepsInstancesBeforeTheNodeRole(t *testing.T) {
	d, f := withSweep(t)

	var sweeping bool
	for i := 0; i < 80 && !d.gone(); i++ {
		msg := d.pass()
		if strings.Contains(msg, "stage 5 of 7, Karpenter: sweeping the account") {
			sweeping = true
			require.Contains(t, msg, "i-ours1")
			require.False(t, d.nodeRoleDeleted(), "the node role waits for the instances")
		}
		if len(f.live()) > 1 { // ours are live, besides theirs
			require.False(t, d.nodeRoleDeleted(), "node role deleted while instances run: %v", d.log)
		}
		if !d.gone() && storedStatus(d.cluster(t)).GetCloudSweep().GetResult() == cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_FINISHED {
			require.Equal(t, []string{"i-theirs"}, f.live())
		}
	}

	require.True(t, d.gone(), "log %v", d.log)
	require.True(t, sweeping, "the deletion said it waits for the sweep")
	require.ElementsMatch(t, []string{"i-ours1", "i-ours2"}, f.terminateCalls, "each terminated once, theirs never")
	require.Equal(t, []string{"i-theirs"}, f.live())
	require.Contains(t, f.regions, d.row.Region)
}

// A forced deletion, which skips the workload stages, still sweeps.
func TestForcedDeletionStillSweeps(t *testing.T) {
	d, f := withSweep(t)
	d.relaxed = true
	row := d.cluster(t)
	row.DeleteForce = true
	require.NoError(t, d.store.Clusters().Update(t.Context(), row))

	d.run(80)

	require.True(t, d.gone(), "log %v", d.log)
	require.Empty(t, d.td.deleted)
	require.ElementsMatch(t, []string{"i-ours1", "i-ours2"}, f.terminateCalls)
}

// An AWS error is the stage's message and is tried again; the sweep then
// finishes.
func TestDeletionSweepRetriesAWSErrors(t *testing.T) {
	d, f := withSweep(t)
	f.failOnce = &smithy.GenericAPIError{Code: "Throttling", Message: "Rate exceeded"}

	messages := d.run(80)

	require.True(t, d.gone(), "log %v", d.log)
	require.Contains(t, strings.Join(messages, "\n"), "Throttling")
	require.Contains(t, strings.Join(messages, "\n"), "trying again")
	require.ElementsMatch(t, []string{"i-ours1", "i-ours2"}, f.terminateCalls)
}

// Instances that never terminate hold the deletion for the teardown
// timeout from the sweep's own start; then it goes on, with a warning
// naming them by ID, and the status keeps what was left.
func TestDeletionSweepTimesOutNamingWhatIsLeft(t *testing.T) {
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	d, f := withSweep(t)
	f.stuck = true

	var last string
	for i := 0; i < 80 && !strings.Contains(last, "sweeping the account"); i++ {
		last = d.pass()
	}
	require.Contains(t, last, "sweeping the account")
	for range 3 {
		require.Contains(t, d.pass(), "sweeping the account", "it waits within the timeout")
	}
	require.False(t, d.nodeRoleDeleted())

	clock = clock.Add(16 * time.Minute)
	d.pass()

	sweep := storedStatus(d.cluster(t)).GetCloudSweep()
	require.Equal(t, cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_TIMED_OUT, sweep.GetResult())
	require.ElementsMatch(t, []string{"i-ours1", "i-ours2"}, sweep.GetWaiting())
	warnings := eventsOf(d.cluster(t), ReasonCloudSweepTimedOut)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "i-ours1")
	d.run(80)
	require.True(t, d.gone())
}

// An imported cluster whose EKS cluster is only observed keeps its EKS
// cluster, so its nodes are not swept.
func TestDeletionOfAnObservedClusterDoesNotSweep(t *testing.T) {
	d, f := withSweep(t)
	d.relaxed = true
	eks := &awseks.Cluster{}
	require.NoError(t, d.kube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: d.prefix}, eks))
	patch := client.MergeFrom(eks.DeepCopy())
	eks.Spec.ManagementPolicies = xpv2.ManagementPolicies{xpv2.ManagementActionObserve}
	require.NoError(t, d.kube.Patch(t.Context(), eks, patch))

	for i := 0; i < 80 && storedStatus(d.cluster(t)).GetCloudSweep() == nil; i++ {
		d.pass()
	}

	require.Equal(t, cloudv1.ClusterTeardownResult_CLUSTER_TEARDOWN_RESULT_SKIPPED, storedStatus(d.cluster(t)).GetCloudSweep().GetResult())
	require.Empty(t, f.terminateCalls)
}

// The node groups' stage's clock is the status's: a restart (the process's
// copy forgotten) does not count the teardown timeout again.
func TestNodeStageClockSurvivesARestart(t *testing.T) {
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	d, _ := withSweep(t)
	d.relaxed = true
	var last string
	for i := 0; i < 80 && !strings.Contains(last, "stage 4 of 7"); i++ {
		last = d.pass()
	}
	require.Contains(t, last, "stage 4 of 7")
	d.td.err = errors.New("connection refused")
	require.Contains(t, d.pass(), "cannot reach the cluster's API")
	start := storedStatus(d.cluster(t)).GetNodeTeardown().GetStartTime()
	require.NotNil(t, start, "the stage's clock is in the status")
	require.Equal(t, clock, start.AsTime())

	clock = clock.Add(10 * time.Minute)
	nodeStageErrors.Delete(d.row.ID) // a restart
	require.Contains(t, d.pass(), "cannot reach the cluster's API")
	require.Equal(t, start.AsTime(), storedStatus(d.cluster(t)).GetNodeTeardown().GetStartTime().AsTime())

	clock = clock.Add(6 * time.Minute) // 16 minutes after the first failure
	nodeStageErrors.Delete(d.row.ID)
	require.NotContains(t, d.pass(), "stage 4 of 7", "the timeout counted from the stored start")
	require.Nil(t, storedStatus(d.cluster(t)).GetNodeTeardown(), "the clock is cleared once the stage is over")
	require.NotEmpty(t, eventsOf(d.cluster(t), reconcile.ReasonTeardownTimedOut))
}
