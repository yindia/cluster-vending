package live

import (
	"errors"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Open resolves only from the tenant and cluster IDs, reads only the live
// kubeconfig, and answers NO_LIVE_IDENTITY when the live identity is not
// there: the controller's kubeconfig (which exists) is never used.
func TestKubeOpener(t *testing.T) {
	const ns = "tenant-acme"
	prefix := crossplaneaws.ClusterPrefix(tenant, clusterID, "prod")
	cluster := func(tenantLabel string) client.Object {
		return &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "prod",
			Labels: map[string]string{cloudstore.LabelID: clusterID, cloudstore.LabelTenant: tenantLabel}}}
	}
	auth := func(ready bool) client.Object {
		a := &awseks.ClusterAuth{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: crossplaneaws.LiveClusterAuthName(prefix)}}
		if ready {
			a.SetConditions(xpv2.Available())
		} else {
			a.SetConditions(xpv2.Unavailable())
		}
		return a
	}
	secret := func(name string, data []byte) client.Object {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string][]byte{crossplaneaws.KubeconfigSecretKey: data}}
	}
	adminKubeconfig := secret(crossplaneaws.KubeconfigSecretName(prefix), []byte("admin"))
	live := secret(crossplaneaws.LiveKubeconfigSecretName(prefix), []byte("live"))
	tests := []struct {
		name    string
		objects []client.Object
		reason  string
	}{
		{name: "no cluster", reason: ReasonUnreachable},
		{name: "another tenant's cluster", objects: []client.Object{cluster("other"), auth(true), live}, reason: ReasonUnreachable},
		{name: "no live ClusterAuth", objects: []client.Object{cluster(tenant), adminKubeconfig}, reason: ReasonNoLiveIdentity},
		{name: "live ClusterAuth not Ready", objects: []client.Object{cluster(tenant), auth(false), adminKubeconfig, live}, reason: ReasonNoLiveIdentity},
		{name: "no live Secret", objects: []client.Object{cluster(tenant), auth(true), adminKubeconfig}, reason: ReasonNoLiveIdentity},
		{name: "empty live Secret", objects: []client.Object{cluster(tenant), auth(true), secret(crossplaneaws.LiveKubeconfigSecretName(prefix), nil)}, reason: ReasonNoLiveIdentity},
		{name: "live identity", objects: []client.Object{cluster(tenant), auth(true), adminKubeconfig, live}},
	}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var used []byte
			o := &KubeOpener{Management: fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build(), Limits: DefaultLimits(),
				NewLister: func(k []byte, _ Limits) (Lister, error) { used = k; return newFake(), nil }}
			_, err := o.Open(t.Context(), tenant, clusterID)
			var le *Error
			switch {
			case tt.reason == "" && err != nil:
				t.Fatal(err)
			case tt.reason == "" && string(used) != "live":
				t.Fatalf("opened with %q, want the live kubeconfig", used)
			case tt.reason != "" && (!errors.As(err, &le) || le.Reason != tt.reason):
				t.Fatalf("err = %v, want %s", err, tt.reason)
			case tt.reason != "" && used != nil:
				t.Fatalf("opened with %q", used)
			}
		})
	}
}
