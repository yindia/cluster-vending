package aws_test

import (
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func TestActivationPolicy(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	policy := crossplaneaws.ActivationPolicy()
	got := make([]string, 0, len(policy.Spec.Activations))
	for _, a := range policy.Spec.Activations {
		got = append(got, string(a))
	}
	// The MRD names as the providers publish them (kubectl get mrd).
	want := []string{
		"accessentries.eks.aws.m.upbound.io", "accesspolicyassociations.eks.aws.m.upbound.io",
		"addons.eks.aws.m.upbound.io", "clusterauths.eks.aws.m.upbound.io", "clusters.eks.aws.m.upbound.io",
		"eips.ec2.aws.m.upbound.io", "internetgateways.ec2.aws.m.upbound.io", "natgateways.ec2.aws.m.upbound.io",
		"nodegroups.eks.aws.m.upbound.io", "podidentityassociations.eks.aws.m.upbound.io",
		"policies.iam.aws.m.upbound.io", "releases.helm.m.crossplane.io", "rolepolicyattachments.iam.aws.m.upbound.io",
		"roles.iam.aws.m.upbound.io", "routes.ec2.aws.m.upbound.io", "routetableassociations.ec2.aws.m.upbound.io",
		"routetables.ec2.aws.m.upbound.io", "subnets.ec2.aws.m.upbound.io", "vpcs.ec2.aws.m.upbound.io",
	}
	if strings.Join(slices.Sorted(slices.Values(got)), ",") != strings.Join(want, ",") {
		t.Errorf("activations\n got %v\nwant %v", got, want)
	}

	// Every kind the builders create is activated, or Crossplane never
	// installs its CRD and the apply fails.
	plan := clusterPlan()
	plan.Addons = []crossplaneaws.AddonPlan{{Name: "aws-ebs-csi-driver"},
		{Name: "aws-mountpoint-s3-csi-driver", S3BucketNames: []string{"b"}}}
	built := append(crossplaneaws.ClusterResources(plan), crossplaneaws.NetworkObjects()...)
	for _, obj := range built {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			t.Fatal(err)
		}
		covered := false
		for _, a := range got {
			plural, group, _ := strings.Cut(a, ".")
			if group == gvk.Group && singular(plural) == strings.ToLower(gvk.Kind) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("%s.%s is built but not activated", gvk.Kind, gvk.Group)
		}
	}
}

// singular undoes the providers' plurals: policies, accessentries, vpcs.
func singular(plural string) string {
	if s, ok := strings.CutSuffix(plural, "ies"); ok {
		return s + "y"
	}
	return strings.TrimSuffix(plural, "s")
}
