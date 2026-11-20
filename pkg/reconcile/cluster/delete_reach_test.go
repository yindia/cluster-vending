package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/stretchr/testify/require"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// setEKSStatus is the status the cluster's EKS cluster reports.
func (d *delOrder) setEKSStatus(status string) {
	d.t.Helper()
	eks := &awseks.Cluster{}
	require.NoError(d.t, d.kube.Get(d.t.Context(), client.ObjectKey{Namespace: worldNS, Name: d.prefix}, eks))
	eks.Status.AtProvider.Status = ptr.To(status)
	require.NoError(d.t, d.kube.Update(d.t.Context(), eks))
}

// letGo counts the Helm Releases taken out of the way (managed without
// Delete).
func (d *delOrder) letGo() int {
	d.t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK.GroupVersion().WithKind("ReleaseList"))
	require.NoError(d.t, d.kube.List(d.t.Context(), list, client.InNamespace(worldNS)))
	n := 0
	for _, r := range list.Items {
		if _, found, _ := unstructured.NestedStringSlice(r.Object, "spec", "managementPolicies"); found {
			n++
		}
	}
	return n
}

// holdReleases keeps every Release the provider is asked to delete for that
// many passes, so a test sees the stage that waits for them.
func (d *delOrder) holdReleases(passes int) {
	for _, o := range d.objects {
		if o.gvk == crossplaneaws.HelmReleaseGVK {
			d.holds[o.name] = passes
		}
	}
}

func (d *delOrder) events() string {
	var out []string
	for _, e := range storedStatus(d.cluster(d.t)).GetRecentEvents() {
		out = append(out, e.GetReason()+": "+e.GetMessage())
	}
	return strings.Join(out, "\n")
}

// An EKS cluster that is UPDATING (an upgrade) still answers: the deletion
// deletes its load balancers, its node groups, and uninstalls the Helm
// Releases, instead of skipping the workload stages and letting the
// Releases go.
func TestDeletionOfAnUpdatingClusterUsesItsAPI(t *testing.T) {
	d := newDelOrder(t)
	d.setEKSStatus("UPDATING")

	for i := 0; i < 80 && !d.gone(); i++ {
		d.pass()
		require.Zero(t, d.letGo(), "no Release is let go without an uninstall while the API answers (pass %d)", i)
	}

	require.True(t, d.gone(), "log %v", d.log)
	require.Contains(t, d.td.deleted, "Service istio-system/gateway", "stage 1 ran")
	require.Contains(t, d.td.deleted, "NodePool gpu", "stage 4 ran")
	require.Contains(t, d.td.deleted, "EC2NodeClass gpu")
}

// The teardown of a cluster whose EKS cluster does not answer says why it
// was skipped (not a blanket "not ACTIVE").
func TestTeardownSkipNamesTheCause(t *testing.T) {
	d := newDelOrder(t)
	d.relaxed = true
	d.setEKSStatus("CREATING")

	d.pass()

	td := storedStatus(d.cluster(t)).GetTeardown()
	require.Equal(t, "CLUSTER_TEARDOWN_RESULT_SKIPPED", td.GetResult().String())
	require.Contains(t, td.GetMessage(), "its EKS cluster is CREATING")
	require.NotContains(t, td.GetMessage(), "not ACTIVE")
	require.Empty(t, d.td.deleted)
}

// An ACTIVE cluster whose API cannot be reached is tried until the teardown
// timeout, then its Helm Releases are let go (with a warning), and the stage
// says why and how to skip the wait; the deletion finishes instead of
// hanging at the platform components.
func TestDeletionLetsReleasesGoWhenTheAPIIsDown(t *testing.T) {
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	d := newDelOrder(t)
	d.relaxed = true
	d.holdReleases(200)
	d.td.err = errors.New("dial tcp 10.0.0.1:443: i/o timeout")

	for range 3 {
		require.Contains(t, d.pass(), "cannot reach the cluster's API", "stage 1 retries until the timeout")
	}
	require.Zero(t, d.letGo())

	clock = clock.Add(16 * time.Minute)
	var msg string
	for i := 0; i < 80 && !d.gone() && !strings.Contains(msg, "skips the wait"); i++ {
		msg = d.pass()
	}

	require.Contains(t, msg, "the cluster's API cannot be reached: dial tcp 10.0.0.1:443: i/o timeout")
	require.Contains(t, msg, "`cluster delete --force`")
	require.Positive(t, d.letGo())
	require.Contains(t, d.events(), reconcile.ReasonReleasesLetGo)
	d.td.err = nil
	d.holdReleases(0)
	d.run(80)
	require.True(t, d.gone(), "log %v", d.log)
}

// An API that goes down after the load balancers are gone is waited for the
// teardown timeout from when it was first seen down, and says so; then the
// Releases go.
func TestDeletionWaitsTheTimeoutBeforeLettingReleasesGo(t *testing.T) {
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	d := newDelOrder(t)
	d.relaxed = true
	for msg := ""; !strings.Contains(msg, "stage 3 of 7"); msg = d.pass() {
	}
	d.holdReleases(200)
	d.td.err = errors.New("connection refused")

	msg := d.pass()
	require.Contains(t, msg, "the cluster's API cannot be reached: connection refused")
	require.Contains(t, msg, "let go without an uninstall in 15m0s")
	require.Contains(t, msg, "`cluster delete --force`")
	require.Zero(t, d.letGo(), "not before the timeout")

	clock = clock.Add(10 * time.Minute)
	require.Contains(t, d.pass(), "in 5m0s")
	require.Zero(t, d.letGo())

	clock = clock.Add(6 * time.Minute)
	d.pass()
	require.Positive(t, d.letGo(), "after the timeout")
	d.td.err = nil
	d.holdReleases(0)
	d.run(80)
	require.True(t, d.gone(), "log %v", d.log)
}

// An API that answers again before the timeout is used as usual: the clock
// starts again, nothing was let go.
func TestDeletionForgetsADownAPIThatCameBack(t *testing.T) {
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = time.Now })
	d := newDelOrder(t)
	d.relaxed = true
	for msg := ""; !strings.Contains(msg, "stage 3 of 7"); msg = d.pass() {
	}
	d.holdReleases(200)
	d.td.err = errors.New("connection refused")
	d.pass()
	clock = clock.Add(10 * time.Minute)
	d.td.err = nil
	d.pass()
	d.td.err = errors.New("connection refused")
	clock = clock.Add(10 * time.Minute)

	msg := d.pass()

	require.Contains(t, msg, "in 15m0s", "the clock counts from the new failure")
	require.Zero(t, d.letGo())
}

// profilesFake is an account's instance profiles holding Karpenter's node role.
type profilesFake struct {
	tags    map[string]map[string]string
	holding map[string]bool
	deleted map[string]bool
}

func (f *profilesFake) ListInstanceProfilesForRole(_ context.Context, in *iam.ListInstanceProfilesForRoleInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	out := &iam.ListInstanceProfilesForRoleOutput{}
	for name := range f.holding {
		n := name
		out.InstanceProfiles = append(out.InstanceProfiles, types.InstanceProfile{InstanceProfileName: &n})
	}
	return out, nil
}

func (f *profilesFake) ListInstanceProfileTags(_ context.Context, in *iam.ListInstanceProfileTagsInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfileTagsOutput, error) {
	out := &iam.ListInstanceProfileTagsOutput{}
	for k, v := range f.tags[*in.InstanceProfileName] {
		out.Tags = append(out.Tags, types.Tag{Key: &k, Value: &v})
	}
	return out, nil
}

func (f *profilesFake) RemoveRoleFromInstanceProfile(_ context.Context, in *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	delete(f.holding, *in.InstanceProfileName)
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

func (f *profilesFake) DeleteInstanceProfile(_ context.Context, in *iam.DeleteInstanceProfileInput, _ ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	f.deleted[*in.InstanceProfileName] = true
	return &iam.DeleteInstanceProfileOutput{}, nil
}

// A forced deletion never deleted the EC2NodeClasses, so Karpenter's
// instance profiles still hold its node role and IAM would refuse to delete
// the role (DeleteConflict). The deletion takes the role out of the profiles
// Karpenter made for this cluster and deletes them, before it deletes the
// role, and touches no other profile.
func TestForcedDeletionRemovesKarpenterInstanceProfilesBeforeTheRole(t *testing.T) {
	d := newDelOrder(t)
	d.relaxed = true
	fake := &profilesFake{deleted: map[string]bool{}, holding: map[string]bool{"karp-prod": true, "karp-staging": true, "by-hand": true},
		tags: map[string]map[string]string{
			"karp-prod":    {awscloud.TagKarpenterNodeClass: "default", awscloud.TagClusterPrefix + "prod": "owned"},
			"karp-staging": {awscloud.TagKarpenterNodeClass: "default", awscloud.TagClusterPrefix + "staging": "owned"},
			"by-hand":      {awscloud.TagClusterPrefix + "prod": "owned"},
		}}
	d.svc.deps.InstanceProfiles = func(a awscloud.Access, region string) (awscloud.InstanceProfileAPI, error) {
		if a.RoleARN == "" || region != "us-east-1" {
			return nil, errors.New("not the tenant role in the cluster's region")
		}
		return fake, nil
	}
	var roleDeletedWithProfile, roleDeleted bool
	d.webhook = func(_ context.Context, _ client.Reader, obj client.Object) error {
		if gvkOf(obj).Kind == awsiam.Role_Kind && obj.GetName() == d.prefix+"-karpenter-node" {
			roleDeleted, roleDeletedWithProfile = true, fake.deleted["karp-prod"] || roleDeletedWithProfile
		}
		return nil
	}
	row := d.cluster(t)
	row.DeleteForce = true
	require.NoError(t, d.store.Clusters().Update(t.Context(), row))

	d.run(80)

	require.True(t, d.gone(), "log %v", d.log)
	require.True(t, roleDeleted, "the node role was deleted")
	require.True(t, roleDeletedWithProfile, "Karpenter's instance profile was deleted before the node role")
	require.True(t, fake.deleted["karp-prod"])
	require.False(t, fake.holding["karp-prod"], "the role was taken out of it")
	require.False(t, fake.deleted["karp-staging"], "another cluster's profile is not ours")
	require.False(t, fake.deleted["by-hand"], "a profile Karpenter did not make is not ours")
	require.True(t, fake.holding["karp-staging"] && fake.holding["by-hand"])
}

// A failing IAM does not hold the deletion: the role's own deletion says
// what is left.
func TestDeletionGoesOnWhenInstanceProfilesCannotBeRead(t *testing.T) {
	d := newDelOrder(t)
	d.relaxed = true
	d.svc.deps.InstanceProfiles = func(awscloud.Access, string) (awscloud.InstanceProfileAPI, error) {
		return nil, errors.New("sts: access denied")
	}

	d.run(80)

	require.True(t, d.gone(), "log %v", d.log)
}
