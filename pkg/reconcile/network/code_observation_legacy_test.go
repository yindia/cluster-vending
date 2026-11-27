package network

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// legacyVPC is a VPC as provider-aws v2.9.0 leaves it: conditions without
// observedGeneration (Condition.Equal keeps a condition written without one
// for as long as its status, reason and message stay the same), with the
// platform's apply and the provider's last status write at the given times
// in its managedFields.
func legacyVPC(applied, statusWritten time.Time, synced string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "ec2.aws.m.upbound.io/v1beta1", "kind": "VPC",
		"metadata": map[string]any{"name": "vpc", "namespace": "tenant-acme", "generation": int64(3)},
		"spec":     map[string]any{"forProvider": map[string]any{}},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "reason": "Available", "lastTransitionTime": "2026-11-16T14:56:55Z"},
			map[string]any{"type": "Synced", "status": synced, "reason": "ReconcileSuccess", "lastTransitionTime": "2026-11-16T14:56:55Z"},
			map[string]any{"type": "LastAsyncOperation", "status": "True", "reason": "Success", "lastTransitionTime": "2026-11-16T14:56:49Z"},
		}}}}
	var fields []metav1.ManagedFieldsEntry
	if !applied.IsZero() {
		fields = append(fields, metav1.ManagedFieldsEntry{Manager: kube.FieldOwner, Operation: metav1.ManagedFieldsOperationApply,
			APIVersion: "ec2.aws.m.upbound.io/v1beta1", Time: &metav1.Time{Time: applied}})
	}
	if !statusWritten.IsZero() {
		fields = append(fields, metav1.ManagedFieldsEntry{Manager: "provider", Operation: metav1.ManagedFieldsOperationUpdate, Subresource: "status",
			APIVersion: "ec2.aws.m.upbound.io/v1beta1", Time: &metav1.Time{Time: statusWritten}})
	}
	object.SetManagedFields(fields)
	return object
}

func TestCodeConvergenceWithoutProviderObservedGeneration(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	for _, test := range []struct {
		name                   string
		applied, statusWritten time.Time
		synced                 string
		want                   bool
		why                    string
	}{
		// The provider wrote its status after the render was applied: its
		// write raced no older read (status updates carry resourceVersion), so
		// the conditions it holds observed the new spec.
		{"status written after the apply", now.Add(-5 * time.Minute), now.Add(-4 * time.Minute), "True", true, ""},
		// A Ready=True cached from before the apply must not finish it.
		{"Ready from before the apply", now.Add(-time.Minute), now.Add(-2 * time.Minute), "True", false, "has not written its status since"},
		{"status written in the apply's second", now.Add(-time.Minute), now.Add(-time.Minute), "True", false, "has not written its status since"},
		// Long past the provider's poll: it observed the object since.
		{"apply older than the provider's poll", now.Add(-providerObservationBound - time.Minute), now.Add(-48 * time.Hour), "True", true, ""},
		{"no apply recorded on it", time.Time{}, now.Add(-time.Minute), "True", false, "no apply by the platform"},
		{"current observation not synced", now.Add(-5 * time.Minute), now.Add(-4 * time.Minute), "False", false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := legacyVPC(test.applied, test.statusWritten, test.synced)
			// The fake client drops managedFields on create; serve them as the
			// apiserver does.
			fields := object.GetManagedFields()
			object.SetManagedFields(nil)
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(object).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					obj.SetManagedFields(fields)
					return nil
				}}).Build()
			record := model.ManagedResource{APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName()}
			s := New(reconcile.Deps{Kube: c, KubeReader: c, Store: codeStore{inventory: codeInventory{records: []model.ManagedResource{record}}}})
			got, err := s.codeResourcesConverged(t.Context(), &model.Network{}, map[string]string{record.APIVersion + "/VPC/tenant-acme/vpc": "hash"})
			if got != test.want {
				t.Fatalf("convergence=%t want %t (err=%v)", got, test.want, err)
			}
			if test.why != "" && (err == nil || !strings.Contains(err.Error(), test.why)) {
				t.Fatalf("err=%v, want it to say %q", err, test.why)
			}
			if test.want && err != nil {
				t.Fatalf("converged with err=%v", err)
			}
		})
	}
}

// The network's objects are EC2 resources: their provider runs with the
// EC2 poll, so an apply counts as observed only once that poll has surely
// run since. A bound shorter than the poll finishes a change on a cached
// Ready=True.
func TestProviderObservationBoundCoversTheEC2Poll(t *testing.T) {
	poll := crossplaneaws.DefaultProviderRuntime().For("provider-aws-ec2").Poll
	if providerObservationBound <= poll {
		t.Errorf("observation bound %s is not longer than the EC2 poll %s", providerObservationBound, poll)
	}
}
