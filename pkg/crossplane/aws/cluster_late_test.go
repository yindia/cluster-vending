package aws_test

import (
	"context"
	"fmt"
	"testing"

	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// seen is what a management cluster has, for a Build: the objects by Go
// type and name, which are Ready, and which the owner has.
type seen struct {
	objects      map[string]client.Object
	ready, owned map[string]bool
}

func seenKey(obj client.Object) string { return fmt.Sprintf("%T/%s", obj, obj.GetName()) }

func (s seen) Get(_ context.Context, obj client.Object) (bool, error) {
	have, ok := s.objects[seenKey(obj)]
	if !ok {
		return false, nil
	}
	switch into := obj.(type) {
	case *awseks.Cluster:
		*into = *have.(*awseks.Cluster)
	case *awskms.Key:
		*into = *have.(*awskms.Key)
	}
	return true, nil
}

func (s seen) Ready(_ context.Context, obj client.Object) (bool, error) {
	return s.ready[seenKey(obj)], nil
}

func (s seen) Owned(obj client.Object) bool { return s.owned[seenKey(obj)] }

// latePlan is a plan with every option that needs a value of another
// resource, and none of those values.
func latePlan() crossplaneaws.ClusterPlan {
	p := clusterPlan()
	p.LogGroupRetentionDays = 90
	p.CreateEncryptionKey = true
	p.IRSA = true
	p.NodePool.LaunchTemplate = &crossplaneaws.LaunchTemplatePlan{SecurityGroupIDs: []string{"sg-0bbb"}}
	return p
}

// built runs every Build of the set against what the management cluster
// has: the objects by name, and what the others wait for.
func built(t *testing.T, set sequence.Set, have seen) (map[string]client.Object, map[string]string) {
	t.Helper()
	objects, waits := map[string]client.Object{}, map[string]string{}
	for _, item := range set {
		if item.Build == nil {
			t.Fatalf("%T %s has no Build", item.Object, item.Object.GetName())
		}
		obj, waitsFor, err := item.Build(t.Context(), have)
		if err != nil {
			t.Fatal(err)
		}
		if obj == nil {
			waits[typeName(item.Object)] = waitsFor
			continue
		}
		objects[obj.GetName()] = obj
	}
	return objects, waits
}

// A plan without late values is the render as it is: nothing is built at
// apply time.
func TestClusterSetWithoutLateValuesBuildsNothing(t *testing.T) {
	for _, item := range crossplaneaws.ClusterSet(clusterPlan()) {
		if item.Build != nil {
			t.Fatalf("%T %s is built at apply time", item.Object, item.Object.GetName())
		}
	}
}

// The set of a plan with late values names every object the cluster will
// have. What the render leaves out for now waits, with the render's own
// reason (Waits); the rest is built with the values that exist.
func TestClusterSetBuildsFromWhatExists(t *testing.T) {
	plan := latePlan()
	full := plan
	full.AssumeLive()
	set := crossplaneaws.ClusterSet(plan)
	if want := crossplaneaws.ClusterResources(full); len(set) != len(want) {
		t.Fatalf("set of %d items, want the %d objects of the full render", len(set), len(want))
	}
	eksName := plan.Prefix
	keyName, logsName := plan.Prefix+"-secrets-key", plan.Prefix+"-logs"

	// A new cluster: nothing exists yet.
	objects, waits := built(t, crossplaneaws.ClusterSet(plan), seen{})
	held := plan
	held.ClusterHeld = true
	for kind, reason := range held.Waits() {
		if waits[kind] != reason {
			t.Errorf("new cluster: %s waits for %q, want %q", kind, waits[kind], reason)
		}
	}
	if waits[typeName(&awseks.Cluster{})] == "" || waits[typeName(&awsiam.OpenIDConnectProvider{})] == "" || waits["Usage"] == "" {
		t.Errorf("new cluster: waits = %v, want the EKS cluster, the OIDC provider and their Usages waiting", waits)
	}
	if objects[keyName] == nil || objects[logsName] == nil || objects[eksName] != nil {
		t.Errorf("new cluster: built %v, want the key and log group and no EKS cluster", keys(objects))
	}

	// The log group is ready and the key has its ARN: the EKS cluster is
	// built with it; what needs the cluster ACTIVE still waits.
	key := &awskms.Key{}
	key.Name = keyName
	key.Status.AtProvider.Arn = ptr.To(testKeyARN)
	have := seen{
		objects: map[string]client.Object{seenKey(key): key},
		ready:   map[string]bool{},
		owned:   map[string]bool{seenKey(key): true},
	}
	group := &awscwl.Group{}
	group.Name = logsName
	have.ready[seenKey(group)] = true
	objects, waits = built(t, crossplaneaws.ClusterSet(plan), have)
	eks, ok := objects[eksName].(*awseks.Cluster)
	if !ok || eks.Spec.ForProvider.EncryptionConfig == nil ||
		ptr.Deref(eks.Spec.ForProvider.EncryptionConfig.Provider.KeyArn, "") != testKeyARN {
		t.Fatalf("EKS cluster built with the key = %v", objects[eksName])
	}
	if waits[typeName(&awseks.Cluster{})] != "" || waits[typeName(&awsiam.OpenIDConnectProvider{})] == "" ||
		waits[typeName(&awsec2.LaunchTemplate{})] == "" || waits[typeName(&awseks.NodeGroup{})] == "" {
		t.Errorf("with the key: waits = %v, want only what needs an ACTIVE cluster", waits)
	}

	// The EKS cluster is ACTIVE: everything is built, with its issuer and
	// security group.
	live := &awseks.Cluster{}
	live.Name = eksName
	live.Status.AtProvider.Status = ptr.To(crossplaneaws.EKSActive)
	live.Status.AtProvider.Identity = []awseks.ClusterIdentityObservation{{Oidc: []awseks.OidcObservation{{Issuer: ptr.To(testIssuer)}}}}
	live.Status.AtProvider.VPCConfig = &awseks.VPCConfigObservation{ClusterSecurityGroupID: ptr.To("sg-0c1u5ter")}
	have.objects[seenKey(live)], have.owned[seenKey(live)] = live, true
	objects, waits = built(t, crossplaneaws.ClusterSet(plan), have)
	if len(waits) != 0 {
		t.Fatalf("active cluster: still waiting: %v", waits)
	}
	filled := plan
	filled.EncryptionKeyARN, filled.OIDCIssuer, filled.ClusterSecurityGroupID = testKeyARN, testIssuer, "sg-0c1u5ter"
	want := crossplaneaws.ClusterResources(filled)
	if len(objects) != len(want) {
		t.Errorf("active cluster: built %d objects, want the %d of the render with the live values", len(objects), len(want))
	}
	for _, o := range want {
		if objects[o.GetName()] == nil {
			t.Errorf("active cluster: %T %s was not built", o, o.GetName())
		}
	}
	if oidc, ok := objects[plan.Prefix+"-oidc"].(*awsiam.OpenIDConnectProvider); !ok || ptr.Deref(oidc.Spec.ForProvider.URL, "") != testIssuer {
		t.Errorf("OIDC provider = %v, want the cluster's issuer", objects[plan.Prefix+"-oidc"])
	}
}

func keys(objects map[string]client.Object) []string {
	out := make([]string, 0, len(objects))
	for name := range objects {
		out = append(out, name)
	}
	return out
}
