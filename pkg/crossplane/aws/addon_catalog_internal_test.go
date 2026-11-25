package aws

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/utils/ptr"
)

// The tables addons.yaml replaced, as they were in Go. The catalog must
// read to exactly these, so every plan renders as before.
var (
	goTopLevel   = [][]string{nil}
	goController = [][]string{{"controller"}}
	goPlacements = map[string]addonPlacement{
		"coredns": {paths: goTopLevel, defaultTolerations: []map[string]any{
			{"key": "CriticalAddonsOnly", "operator": "Exists"},
			{"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"},
		}},
		"aws-ebs-csi-driver": {paths: goController, defaultTolerations: []map[string]any{
			{"key": "CriticalAddonsOnly", "operator": "Exists"},
			{"effect": "NoExecute", "operator": "Exists", "tolerationSeconds": 300},
		}},
		"metrics-server":               {paths: goTopLevel},
		"kube-state-metrics":           {paths: goTopLevel},
		"snapshot-controller":          {paths: goTopLevel},
		"cert-manager":                 {paths: [][]string{nil, {"cainjector"}, {"webhook"}}},
		"aws-efs-csi-driver":           {paths: goController},
		"aws-fsx-csi-driver":           {paths: goController},
		"aws-mountpoint-s3-csi-driver": {paths: goController},
	}
	goSetups = map[string]addonSetup{
		"aws-ebs-csi-driver": {short: "ebs-csi", namespace: "kube-system", serviceAccount: "ebs-csi-controller-sa",
			managedPolicies: []string{"arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"}, protectIAM: true},
		"aws-efs-csi-driver": {short: "efs-csi", namespace: "kube-system", serviceAccount: "efs-csi-controller-sa",
			managedPolicies: []string{"arn:aws:iam::aws:policy/service-role/AmazonEFSCSIDriverPolicy"}},
		"aws-fsx-csi-driver": {short: "fsx-csi", namespace: "kube-system", serviceAccount: "fsx-csi-controller-sa",
			managedPolicies: []string{"arn:aws:iam::aws:policy/AmazonFSxFullAccess"}},
		"aws-mountpoint-s3-csi-driver": {short: "s3-csi", namespace: "kube-system", serviceAccount: "s3-csi-driver-sa",
			hook: hookS3BucketPolicy},
	}
	goDefaults = []string{"vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent", "aws-ebs-csi-driver"}
)

// jsonOf is v as the renderer writes it (json.Marshal): numbers from YAML
// (float64) and from Go (int) write the same.
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAddonCatalogIsTheGoTablesItReplaced(t *testing.T) {
	c := addonCatalog

	if !slices.Equal(c.Defaults(), goDefaults) {
		t.Errorf("defaults = %v, want %v", c.Defaults(), goDefaults)
	}
	if len(c.placements) != len(goPlacements) {
		t.Errorf("%d placed add-ons, want %d", len(c.placements), len(goPlacements))
	}
	for name, want := range goPlacements {
		got, ok := c.placements[name]
		if !ok {
			t.Errorf("%s: not placed", name)
			continue
		}
		if !reflect.DeepEqual(got.paths, want.paths) {
			t.Errorf("%s: paths %v, want %v", name, got.paths, want.paths)
		}
		if g, w := jsonOf(t, got.defaultTolerations), jsonOf(t, want.defaultTolerations); g != w {
			t.Errorf("%s: default tolerations %s, want %s", name, g, w)
		}
	}
	if !reflect.DeepEqual(c.setups, goSetups) {
		t.Errorf("setups = %+v\nwant     %+v", c.setups, goSetups)
	}
}

// withCatalog renders with c in place of the shipped catalog.
func withCatalog(t *testing.T, c AddonCatalog) {
	t.Helper()
	shipped := addonCatalog
	addonCatalog = c
	t.Cleanup(func() { addonCatalog = shipped })
}

// A new add-on is an entry in addons.yaml: placed, and given its role and
// pod identity, with no Go change.
func TestANewAddonEntryNeedsNoGoChange(t *testing.T) {
	extra := `
  - name: example-operator
    placement:
      paths: [., operator.manager]
      defaultTolerations:
        - {key: example.com/critical, operator: Exists}
    schema: {version: v1.0.0-eksbuild.1, source: a test}
    setup:
      short: example
      namespace: example-system
      serviceAccount: example-operator
      managedPolicies: [arn:aws:iam::aws:policy/ReadOnlyAccess]
`
	c, err := ParseAddonCatalog(append(slices.Clone(addonsFile), extra...))
	if err != nil {
		t.Fatal(err)
	}
	withCatalog(t, c)
	p := ClusterPlan{Namespace: "t", Prefix: "p", ClusterName: "c", Region: "us-east-1",
		NodePool: NodePoolPlan{Labels: map[string]string{"role": "system"},
			Taints: []TaintPlan{{Key: "dedicated", Value: "system", Effect: "NO_SCHEDULE"}}},
		Addons: []AddonPlan{{Name: "example-operator", ConfigurationValues: `{"operator":{"replicas":2}}`}}}

	var addon *awseks.Addon
	var role, attachment, identity bool
	for _, o := range ClusterResources(p) {
		switch o := o.(type) {
		case *awseks.Addon:
			addon = o
		case *awsiam.Role:
			role = role || o.GetName() == "p-example-role"
		case *awsiam.RolePolicyAttachment:
			attachment = attachment || (o.GetName() == "p-example-policy" &&
				ptr.Deref(o.Spec.ForProvider.PolicyArn, "") == "arn:aws:iam::aws:policy/ReadOnlyAccess")
		case *awseks.PodIdentityAssociation:
			identity = identity || ptr.Deref(o.Spec.ForProvider.ServiceAccount, "") == "example-operator"
		}
	}

	if addon == nil {
		t.Fatal("no Addon rendered")
	}
	tolerations := `[{"effect":"NoSchedule","key":"dedicated","operator":"Equal","value":"system"},` +
		`{"key":"example.com/critical","operator":"Exists"}]`
	want := `{"nodeSelector":{"role":"system"},"operator":{"manager":{"nodeSelector":{"role":"system"},"tolerations":` +
		tolerations + `},"replicas":2},"tolerations":` + tolerations + `}`
	if got := ptr.Deref(addon.Spec.ForProvider.ConfigurationValues, ""); got != want {
		t.Errorf("configurationValues\ngot  %s\nwant %s", got, want)
	}
	if !role || !attachment || !identity {
		t.Errorf("setup: role %v, policy attachment %v, pod identity %v; want all", role, attachment, identity)
	}
}
