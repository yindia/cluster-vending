package owner_test

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/owner"
)

const tenant = "tenant-acme"

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := cloudv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func account(name string) *cloudv1alpha1.CloudAccount {
	return &cloudv1alpha1.CloudAccount{ObjectMeta: metav1.ObjectMeta{Namespace: tenant, Name: name, UID: types.UID("uid-" + name)}}
}

func network(name, accountName string) *cloudv1alpha1.Network {
	return &cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Namespace: tenant, Name: name, UID: types.UID("uid-" + name)},
		Spec: cloudv1alpha1.NetworkSpec{AccountRef: cloudv1alpha1.LocalReference{Name: accountName}}}
}

func cluster(name, networkName string) *cloudv1alpha1.Cluster {
	return &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: tenant, Name: name},
		Spec: cloudv1alpha1.ClusterSpec{Network: cloudv1alpha1.ClusterNetwork{NetworkRef: cloudv1alpha1.LocalReference{Name: networkName}}}}
}

func component(clusterName, name string) *cloudv1alpha1.Component {
	return &cloudv1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Namespace: tenant, Name: clusterName + "-" + name},
		Spec: cloudv1alpha1.ComponentSpec{Component: name}}
}

// A Cluster owns its Components: each named <cluster>-<component>, labelled
// with the chain above it.
func TestSetClusterOwnsItsComponents(t *testing.T) {
	s := scheme(t)
	c := cluster("prod", "net")
	c.UID = types.UID("uid-prod")
	c.Labels = map[string]string{cloudv1alpha1.LabelAccount: "production"}
	karpenter := component("prod", "karpenter")
	if err := owner.Set(c, karpenter, s); err != nil {
		t.Fatal(err)
	}
	refs := karpenter.GetOwnerReferences()
	if len(refs) != 1 || refs[0].Kind != "Cluster" || refs[0].Name != "prod" || refs[0].Controller == nil || !*refs[0].Controller {
		t.Errorf("owner references = %+v, want the Cluster prod as controller", refs)
	}
	want := map[string]string{cloudv1alpha1.LabelCluster: "prod", cloudv1alpha1.LabelNetwork: "net", cloudv1alpha1.LabelAccount: "production"}
	for k, v := range want {
		if karpenter.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, karpenter.Labels[k], v)
		}
	}
	for name, tt := range map[string]struct {
		owner     client.Object
		dependent *cloudv1alpha1.Component
	}{
		"another cluster's name": {cluster("prod", "net"), component("staging", "karpenter")},
		"network -> component":   {network("net", "production"), component("net", "karpenter")},
	} {
		if err := owner.Set(tt.owner, tt.dependent, s); err == nil || !strings.Contains(err.Error(), "is not the owner") {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
}

// The chain is CloudAccount -> Network -> Cluster: each dependent gets one
// controller owner reference that blocks its owner's deletion, and the
// labels its owners are found by.
func TestSetBuildsTheOwnershipChain(t *testing.T) {
	s := scheme(t)
	acct, net, c := account("production"), network("prod", "production"), cluster("prod", "prod")
	net.Labels = map[string]string{"kept": "yes"}
	if err := owner.Set(acct, net, s); err != nil {
		t.Fatal(err)
	}
	if err := owner.Set(net, c, s); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		dependent    client.Object
		ownerKind    string
		ownerName    string
		wantedLabels map[string]string
	}{
		{net, "CloudAccount", "production", map[string]string{cloudv1alpha1.LabelAccount: "production", "kept": "yes"}},
		{c, "Network", "prod", map[string]string{cloudv1alpha1.LabelAccount: "production", cloudv1alpha1.LabelNetwork: "prod"}},
	} {
		refs := tt.dependent.GetOwnerReferences()
		if len(refs) != 1 {
			t.Fatalf("%T has %d owner references, want 1", tt.dependent, len(refs))
		}
		ref := refs[0]
		if ref.Kind != tt.ownerKind || ref.Name != tt.ownerName || ref.APIVersion != cloudv1alpha1.GroupVersion.String() ||
			ref.Controller == nil || !*ref.Controller || ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
			t.Errorf("%T owner reference = %+v, want a blocking controller reference to %s %s", tt.dependent, ref, tt.ownerKind, tt.ownerName)
		}
		for k, v := range tt.wantedLabels {
			if tt.dependent.GetLabels()[k] != v {
				t.Errorf("%T label %s = %q, want %q", tt.dependent, k, tt.dependent.GetLabels()[k], v)
			}
		}
	}
	// One account owns many networks, one network many clusters.
	if err := owner.Set(acct, network("staging", "production"), s); err != nil {
		t.Errorf("a second network of the account: %v", err)
	}
	if err := owner.Set(net, cluster("batch", "prod"), s); err != nil {
		t.Errorf("a second cluster of the network: %v", err)
	}
}

func TestSetRefusesWhatIsNotTheChain(t *testing.T) {
	s := scheme(t)
	elsewhere := network("prod", "production")
	elsewhere.Namespace = "tenant-other"
	for name, tt := range map[string]struct {
		owner, dependent client.Object
		want             string
	}{
		"another account":    {account("staging"), network("prod", "production"), "accountRef production"},
		"another network":    {network("other", "production"), cluster("prod", "prod"), "networkRef prod"},
		"account -> cluster": {account("production"), cluster("prod", "prod"), "is not the owner"},
		"cluster -> network": {cluster("prod", "prod"), network("prod", "production"), "is not the owner"},
		"another namespace":  {account("production"), elsewhere, "share a namespace"},
		"an account's owner": {network("prod", "production"), account("production"), "has no owner"},
	} {
		err := owner.Set(tt.owner, tt.dependent, s)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, tt.want)
		}
		if len(tt.dependent.GetOwnerReferences()) != 0 {
			t.Errorf("%s: an owner reference was set", name)
		}
	}
}
