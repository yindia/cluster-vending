package controller_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/controller"
	cloudv1alpha1 "github.com/yindia/cluster-vending/controller/api/v1alpha1"
)

// The manager is built, with its three controllers and probes, without
// contacting a cluster.
func TestNewManagerRegistersTheControllers(t *testing.T) {
	mgr, err := controller.NewManager(controller.Options{
		Config: &rest.Config{Host: "http://127.0.0.1:1"}, MetricsAddr: "0", ProbeAddr: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"CloudAccount", "Network", "Cluster"} {
		if !mgr.GetScheme().Recognizes(cloudv1alpha1.GroupVersion.WithKind(kind)) {
			t.Errorf("the manager's scheme does not know %s", kind)
		}
	}
}

// crd reads a generated CRD.
func crd(t *testing.T, plural string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("config", "crd", "bases", "cloud.cluster-vending.io_"+plural+".yaml"))
	if err != nil {
		t.Fatalf("%v (run make controller-apis)", err)
	}
	out := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	return out
}

// rules collects every CEL rule of a schema, by the path of the field it
// is on.
func rules(prefix string, s apiextensionsv1.JSONSchemaProps, out map[string][]string) {
	for _, v := range s.XValidations {
		out[prefix] = append(out[prefix], v.Rule)
	}
	for name, p := range s.Properties {
		rules(strings.TrimPrefix(prefix+"."+name, "."), p, out)
	}
	if s.Items != nil && s.Items.Schema != nil {
		rules(prefix+"[]", *s.Items.Schema, out)
	}
}

func has(all []string, part string) bool {
	for _, r := range all {
		if strings.Contains(r, part) {
			return true
		}
	}
	return false
}

// The generated CRDs are namespaced kinds of the controller's group with a
// status subresource, and carry what the contract says cannot change: the
// ownership references above all.
func TestGeneratedCRDs(t *testing.T) {
	for plural, tt := range map[string]struct {
		kind      string
		shortName string
		immutable map[string]string // field path -> part of its rule
	}{
		"cloudaccounts": {"CloudAccount", "cvacct", map[string]string{
			"spec.aws.accountID": "self == oldSelf",
		}},
		"networks": {"Network", "cvnet", map[string]string{
			"spec.accountRef": "self == oldSelf",
			"spec.region":     "self == oldSelf",
			"spec":            "self.layout == oldSelf.layout",
			"spec.importFrom": "self.aws == oldSelf.aws",
			"spec.flowLogs":   "self.trafficType == oldSelf.trafficType",
			"":                "self.metadata.name.matches",
		}},
		"clusters": {"Cluster", "cvc", map[string]string{
			"spec.network":                        "self == oldSelf",
			"spec.region":                         "self == oldSelf",
			"spec.controlPlane.kubernetesVersion": "int(self.split('.')[1]) >= int(oldSelf.split('.')[1])",
			"spec.controlPlane.secretsEncryption": "self.kmsKeyARN == oldSelf.kmsKeyARN",
			"spec.aws.additionalSecurityGroupIDs": "self == oldSelf",
			"spec.importFrom":                     "!oldSelf.takeOver",
			"spec":                                "has(self.importFrom) == has(oldSelf.importFrom)",
			"":                                    "self.metadata.name.matches",
		}},
	} {
		def := crd(t, plural)
		if def.Spec.Group != cloudv1alpha1.GroupVersion.Group || def.Spec.Names.Kind != tt.kind ||
			def.Spec.Scope != apiextensionsv1.NamespaceScoped || len(def.Spec.Versions) != 1 {
			t.Errorf("%s: group %s, kind %s, scope %s, %d versions", plural, def.Spec.Group, def.Spec.Names.Kind,
				def.Spec.Scope, len(def.Spec.Versions))
			continue
		}
		if !has(def.Spec.Names.ShortNames, tt.shortName) {
			t.Errorf("%s: short names %v, want %s", plural, def.Spec.Names.ShortNames, tt.shortName)
		}
		version := def.Spec.Versions[0]
		if version.Name != cloudv1alpha1.GroupVersion.Version || version.Subresources == nil || version.Subresources.Status == nil {
			t.Errorf("%s: version %s without a status subresource", plural, version.Name)
		}
		found := map[string][]string{}
		rules("", *version.Schema.OpenAPIV3Schema, found)
		for path, part := range tt.immutable {
			if !has(found[path], part) {
				t.Errorf("%s: no rule containing %q on %q (rules there: %v)", plural, part, path, found[path])
			}
		}
	}
}

// The samples are objects of the API that reference each other along the
// ownership chain.
func TestSamplesFormTheChain(t *testing.T) {
	read := func(name string, into any) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("config", "samples", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.UnmarshalStrict(raw, into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	account, network, cluster := &cloudv1alpha1.CloudAccount{}, &cloudv1alpha1.Network{}, &cloudv1alpha1.Cluster{}
	read("cloud_v1alpha1_cloudaccount.yaml", account)
	read("cloud_v1alpha1_network.yaml", network)
	read("cloud_v1alpha1_cluster.yaml", cluster)
	if network.Spec.AccountRef.Name != account.Name || network.Namespace != account.Namespace {
		t.Errorf("the network names account %q in %q, the account is %q in %q",
			network.Spec.AccountRef.Name, network.Namespace, account.Name, account.Namespace)
	}
	if cluster.Spec.Network.NetworkRef.Name != network.Name || cluster.Namespace != network.Namespace {
		t.Errorf("the cluster names network %q in %q, the network is %q in %q",
			cluster.Spec.Network.NetworkRef.Name, cluster.Namespace, network.Name, network.Namespace)
	}
	if cluster.Spec.ReleaseChannel != cloudv1alpha1.ReleaseChannelStable || cluster.Spec.Tier != cloudv1alpha1.ClusterTier0 {
		t.Errorf("release channel %q, tier %q", cluster.Spec.ReleaseChannel, cluster.Spec.Tier)
	}
}
