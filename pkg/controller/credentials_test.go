package controller

import (
	"strings"
	"testing"

	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// With the providers' credentials Secret set but missing, the account says
// so on its status and nothing is applied; once the Secret exists the next
// pass applies the Secret-based ProviderConfig.
func TestCloudAccountReportsAMissingCredentialsSecret(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.deps.ProviderCredentials = &crossplaneaws.CredentialsSecret{Namespace: "crossplane-system", Name: "aws-local", Key: "credentials"}
	r := NewCloudAccountReconciler(w.deps)
	key := client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}
	req := ctrl.Request{NamespacedName: key}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	obj := &cloudv1alpha1.CloudAccount{}
	w.must(t, w.k.Get(ctx, key, obj))
	cond := meta.FindStatusCondition(obj.Status.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "crossplane-system/aws-local") {
		t.Fatalf("Ready condition = %+v, want False naming the Secret", cond)
	}
	pcKey := client.ObjectKey{Namespace: namespace, Name: "aws-0a1b2c3d"}
	if err := w.k.Get(ctx, pcKey, &awsv1beta1.ProviderConfig{}); err == nil {
		t.Error("a ProviderConfig was applied without its credentials Secret")
	}

	w.must(t, w.k.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "crossplane-system", Name: "aws-local"},
		Data: map[string][]byte{"credentials": []byte("[default]\n")}}))
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pc := &awsv1beta1.ProviderConfig{}
	w.must(t, w.k.Get(ctx, pcKey, pc))
	if pc.Spec.Credentials.Source != "Secret" || pc.Spec.Credentials.SecretRef == nil {
		t.Errorf("credentials = %+v, want the Secret", pc.Spec.Credentials)
	}
	w.must(t, w.k.Get(ctx, key, obj))
	if cond := meta.FindStatusCondition(obj.Status.Conditions, ConditionReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition after the Secret exists = %+v", cond)
	}
}
