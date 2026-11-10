package cluster

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

func addonNames(addons []*cloudv1.AwsAddon) []string {
	out := make([]string, 0, len(addons))
	for _, a := range addons {
		out = append(out, a.GetName())
	}
	return out
}

func TestMergeAddons(t *testing.T) {
	got := mergeAddons([]*cloudv1.AwsAddon{
		{Name: "aws-efs-csi-driver"}, {Name: "coredns", ConfigurationValues: `{"replicaCount":3}`},
	})
	want := []string{"vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent", "aws-ebs-csi-driver", "aws-efs-csi-driver"}
	if !equal(addonNames(got), want) {
		t.Fatalf("merged = %v, want %v", addonNames(got), want)
	}
	if got[1].GetConfigurationValues() != `{"replicaCount":3}` {
		t.Error("an entry with a default's name must replace the default")
	}
}

func versionService() *ClusterService {
	return NewClusterService(service.Deps{AWSClusters: fakeLookup{},
		AWS: config.AWS{StaticKeys: &config.StaticKeys{AccessKeyID: "AKIA", SecretAccessKey: "x"}}})
}

func versionsOf(spec *cloudv1.ClusterSpec) map[string]string {
	out := map[string]string{}
	for _, a := range spec.GetAws().GetAddons() {
		out[a.GetName()] = a.GetVersion()
	}
	return out
}

func TestResolveVersionsOnCreate(t *testing.T) {
	in := minimalSpec()
	in.GetAws().Addons = []*cloudv1.AwsAddon{{Name: "coredns", Version: "v0-1.34"}}
	spec := resolveSpec(in, []string{"use1-az1", "use1-az2"})
	err := versionService().resolveVersions(context.Background(), &model.CloudAccount{}, spec, pinnedAddons(in))
	if err != nil {
		t.Fatal(err)
	}
	v := versionsOf(spec)
	if spec.GetControlPlane().GetKubernetesVersion() != "1.34" || v["coredns"] != "v0-1.34" || v["vpc-cni"] != "v1-1.34" {
		t.Errorf("kubernetes %s, add-ons %v", spec.GetControlPlane().GetKubernetesVersion(), v)
	}
}

func TestUpgradeMovesOnlyUnpinnedAddons(t *testing.T) {
	// Created on 1.34 with coredns pinned; upgraded to 1.35.
	in := minimalSpec()
	in.GetAws().Addons = []*cloudv1.AwsAddon{{Name: "coredns", Version: "v0-1.34"}}
	spec := resolveSpec(in, []string{"use1-az1", "use1-az2"})
	s := versionService()
	if err := s.resolveVersions(context.Background(), &model.CloudAccount{}, spec, pinnedAddons(in)); err != nil {
		t.Fatal(err)
	}
	input, _ := encode(&cloudv1.Cluster{Spec: in})
	row := &model.Cluster{Input: input}
	spec.ControlPlane.KubernetesVersion = "1.35"

	// The pinned coredns is not compatible with 1.35: refused.
	err := s.reresolveVersions(context.Background(), row, &model.CloudAccount{}, spec)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err = %v, want invalid_argument for the pinned coredns", err)
	}

	// Unpinned (no version in the input): every add-on follows 1.35.
	in.GetAws().Addons = nil
	input, _ = encode(&cloudv1.Cluster{Spec: in})
	row.Input = input
	if err := s.reresolveVersions(context.Background(), row, &model.CloudAccount{}, spec); err != nil {
		t.Fatal(err)
	}
	for name, v := range versionsOf(spec) {
		if v != "v1-1.35" {
			t.Errorf("%s = %s, want v1-1.35", name, v)
		}
	}
}
