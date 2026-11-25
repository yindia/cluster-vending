package network

import (
	"context"
	"strings"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/conditions"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func TestPinnedRuntimePublishesSyncedObservedGeneration(t *testing.T) {
	resource := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Generation: 42}}
	manager := conditions.ObservedGenerationPropagationManager{}
	manager.For(resource).MarkConditions(xpv2.ReconcileSuccess())
	if got := resource.GetCondition(xpv2.TypeSynced).ObservedGeneration; got != 42 {
		t.Fatalf("pinned provider runtime observedGeneration=%d, want 42", got)
	}
}

type codeInventory struct {
	repository.ManagedResources
	records []model.ManagedResource
}

func (i codeInventory) ListByOwner(context.Context, model.OwnerKind, string) ([]model.ManagedResource, error) {
	return i.records, nil
}

type codeStore struct {
	repository.Store
	inventory codeInventory
}

func (s codeStore) ManagedResources() repository.ManagedResources { return s.inventory }

func TestCodeConvergenceRequiresPinnedProviderObservedGeneration(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	// provider-upjet-aws v2.9.0 pins Crossplane Runtime commit 9b2fb6b1d1ff.
	// Its managed reconciler installs ObservedGenerationPropagationManager at
	// reconciler.go:988; pkg/conditions/manager.go MarkConditions stamps Synced
	// with the observed generation. Conditions without one are read by
	// observedSinceApply; with no platform apply on record they fail closed.
	for _, test := range []struct {
		name                    string
		observed, readyObserved int64
		async                   string
		want                    bool
		why                     string
	}{
		{"missing legacy observation", 0, 0, "True", false, "no apply by the platform"},
		{"old Ready and old Synced", 1, 1, "True", false, ""},
		{"old explicit Ready generation", 2, 1, "True", false, ""},
		{"async still running", 2, 0, "False", false, ""},
		{"old Ready despite current Synced failure", 2, 0, "True", false, ""},
		{"supported current Synced and finished async", 2, 0, "True", true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "ec2.aws.m.upbound.io/v1beta1", "kind": "VPC", "metadata": map[string]any{"name": "vpc", "namespace": "tenant-acme", "generation": int64(2)}, "spec": map[string]any{"forProvider": map[string]any{}}, "status": map[string]any{"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "reason": "Available", "observedGeneration": test.readyObserved},
				map[string]any{"type": "Synced", "status": "True", "reason": "ReconcileSuccess", "observedGeneration": test.observed},
				map[string]any{"type": "AsyncOperation", "status": test.async},
			}}}}
			if test.name == "old Ready despite current Synced failure" {
				object.Object["status"].(map[string]any)["conditions"].([]any)[1].(map[string]any)["status"] = "False"
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(object).Build()
			record := model.ManagedResource{APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName()}
			s := New(reconcile.Deps{Kube: c, KubeReader: c, Store: codeStore{inventory: codeInventory{records: []model.ManagedResource{record}}}})
			got, err := s.codeResourcesConverged(t.Context(), &model.Network{}, map[string]string{record.APIVersion + "/VPC/tenant-acme/vpc": "hash"})
			if got != test.want || (test.why != "" && (err == nil || !strings.Contains(err.Error(), test.why))) {
				t.Fatalf("convergence=%t err=%v", got, err)
			}
		})
	}
}
