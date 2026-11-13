package management_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/management"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func helmSecret(namespace, name string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels}}
}

// The Helm storage lists the records of one release in one namespace, as
// Helm labels them, and removes exactly the record named.
func TestHelmStorage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		helmSecret("kyverno", "sh.helm.release.v1.kyverno.v1", map[string]string{"owner": "helm", "name": "kyverno", "status": "superseded", "version": "1", "createdAt": "1794500000"}),
		helmSecret("kyverno", "sh.helm.release.v1.kyverno.v2", map[string]string{"owner": "helm", "name": "kyverno", "status": "pending-upgrade", "version": "2",
			"createdAt": "1794500000", "modifiedAt": "1794503600"}),
		helmSecret("kyverno", "sh.helm.release.v1.other.v1", map[string]string{"owner": "helm", "name": "other", "status": "deployed", "version": "1"}),
		helmSecret("elsewhere", "sh.helm.release.v1.kyverno.v1", map[string]string{"owner": "helm", "name": "kyverno", "status": "deployed", "version": "1"}),
		helmSecret("kyverno", "not-helm", map[string]string{"name": "kyverno"}),
		helmSecret("kyverno", "odd", map[string]string{"owner": "helm", "name": "kyverno", "version": "two"}),
	).Build()
	storage := management.HelmStorageOf(c)

	records, err := storage.Records(t.Context(), "kyverno", "kyverno")

	if err != nil {
		t.Fatal(err)
	}
	want := map[string]reconcile.HelmRecord{
		"sh.helm.release.v1.kyverno.v1": {Name: "sh.helm.release.v1.kyverno.v1", Revision: 1, Status: "superseded", Changed: time.Unix(1794500000, 0)},
		"sh.helm.release.v1.kyverno.v2": {Name: "sh.helm.release.v1.kyverno.v2", Revision: 2, Status: "pending-upgrade", Changed: time.Unix(1794503600, 0)},
	}
	if len(records) != len(want) {
		t.Fatalf("records = %+v, want %d", records, len(want))
	}
	for _, r := range records {
		if w := want[r.Name]; !r.Changed.Equal(w.Changed) || r.Revision != w.Revision || r.Status != w.Status {
			t.Errorf("record %+v, want %+v", r, w)
		}
	}

	if err := storage.Remove(t.Context(), "kyverno", "sh.helm.release.v1.kyverno.v2"); err != nil {
		t.Fatal(err)
	}
	if err := storage.Remove(t.Context(), "kyverno", "sh.helm.release.v1.kyverno.v2"); err != nil {
		t.Fatalf("removing a record that is gone: %v", err)
	}
	left := &corev1.SecretList{}
	if err := c.List(t.Context(), left, client.InNamespace("kyverno")); err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 4 {
		t.Fatalf("%d Secrets left in the namespace, want the 4 that were not named", len(left.Items))
	}
}
