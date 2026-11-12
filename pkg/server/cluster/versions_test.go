package cluster

import (
	"context"
	"fmt"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// fakeLookup answers like EKS: default Kubernetes 1.34, add-on versions
// "v1-<k8s>" (the recommended one) and "v0-<k8s>". Only the reconciler
// may ask it: the handlers are built without AWS lookups.
type fakeLookup struct{ exists bool }

func (f fakeLookup) EKSClusterExists(context.Context, awscloud.Access, string, string) (bool, error) {
	return f.exists, nil
}

func (f fakeLookup) ResolveEKSVersions(_ context.Context, _ awscloud.Access, _ string, req awscloud.VersionRequest) (awscloud.VersionResult, error) {
	k8s := req.KubernetesVersion
	if k8s == "" {
		k8s = "1.34"
	}
	out := awscloud.VersionResult{KubernetesVersion: k8s, Addons: map[string]awscloud.AddonVersion{}}
	for _, a := range req.Addons {
		switch a.Version {
		case "":
			out.Addons[a.Name] = awscloud.AddonVersion{Version: "v1-" + k8s}
		case "v1-" + k8s, "v0-" + k8s:
			out.Addons[a.Name] = awscloud.AddonVersion{Version: a.Version}
		default:
			return awscloud.VersionResult{}, fmt.Errorf("%w: %s %s not compatible with %s", awscloud.ErrInvalid, a.Name, a.Version, k8s)
		}
	}
	return out, nil
}

func versionsOf(spec *cloudv1.ClusterSpec) map[string]string {
	out := map[string]string{}
	for _, a := range spec.GetAws().GetAddons() {
		out[a.GetName()] = a.GetVersion()
	}
	return out
}

// A plan made before the reconciler resolved the versions shows what is
// known: a pinned add-on its pin, an unpinned one "(known after apply)"
// when the Kubernetes version moves or the add-on is new, and the version
// it has when nothing about it changes.
func TestPlanShowsUnresolvedVersionsAsKnownAfterApply(t *testing.T) {
	in := minimalSpec()
	in.GetAws().Addons = []*cloudv1.AwsAddon{{Name: "coredns", Version: "v0-1.34"}}
	input, err := reconcilecluster.Encode(&cloudv1.Cluster{Spec: in})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.Cluster{Input: input}
	spec := mustResolve(t, in, []string{"use1-az1", "use1-az2"})
	spec.ControlPlane.KubernetesVersion = "1.34"
	for _, a := range spec.GetAws().GetAddons() {
		if a.GetName() == "vpc-cni" {
			a.Version = "v1-1.34" // resolved before
		}
	}

	same, err := withUnknownVersions(row, spec, "1.34")
	if err != nil {
		t.Fatal(err)
	}
	v := versionsOf(same)
	if v["coredns"] != "v0-1.34" || v["vpc-cni"] != "v1-1.34" || v["kube-proxy"] != crossplaneaws.KnownAfterApply {
		t.Errorf("same Kubernetes version: %v; want the pin, the version it has, and a new add-on unknown", v)
	}
	if versionsOf(spec)["kube-proxy"] != "" {
		t.Error("the spec itself was changed")
	}

	moved, err := withUnknownVersions(row, spec, "1.33")
	if err != nil {
		t.Fatal(err)
	}
	v = versionsOf(moved)
	if v["coredns"] != "v0-1.34" || v["vpc-cni"] != crossplaneaws.KnownAfterApply {
		t.Errorf("Kubernetes version moved: %v; want the pin kept and the unpinned unknown", v)
	}
}

func TestCheckVersionStep(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil) // the EKS cluster reports no version: the stored one counts
	tests := []struct {
		stored, target string
		wantErr        bool
	}{
		{"1.31", "1.32", false},
		{"1.31", "1.31", false},
		{"1.31", "1.33", true},
		{"1.31", "1.30", true},
		{"", "1.33", false},
		{"1.31", "", false},
	}
	for _, tt := range tests {
		err := w.svc.CheckVersionStep(t.Context(), w.row, tt.stored, tt.target)
		if (err != nil) != tt.wantErr {
			t.Errorf("%q -> %q: %v, wantErr %v", tt.stored, tt.target, err, tt.wantErr)
		}
	}
}
