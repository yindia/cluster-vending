package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func definitionClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&cloudv1alpha1.ComponentDefinition{}).Build()
}

// unstructuredOnly is c as the manager's cache answers: managed resources
// (Crossplane kinds) are cached as unstructured objects only, so a typed
// list of one fails the way the real cache does.
func unstructuredOnly(c client.Client) client.Client {
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*unstructured.UnstructuredList); !ok && strings.Contains(reflect.TypeOf(list).Elem().PkgPath(), "provider-aws") {
				return fmt.Errorf("%T is not cached", list)
			}
			return cl.List(ctx, list, opts...)
		},
	})
}

// The recorded objects are read the way the cache holds them: a cluster's
// Roles hold the definition, and reading them does not fail.
func TestComponentDefinitionReadsRolesAsTheCacheHoldsThem(t *testing.T) {
	lb := catalogEntry("lb", cloudv1alpha1.ComponentDefinitionSpec{Management: cloudv1alpha1.ComponentManagementPlatform})
	role := &awsiam.Role{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "prod-lb-role",
		Labels: map[string]string{crossplane.LabelComponent: "lb", crossplane.LabelCluster: holderClusterID}}}
	c := unstructuredOnly(definitionClient(t, lb, holderCluster(nil), role))
	r := &ComponentDefinitionReconciler{c: c}

	got := deleteAndReconcile(t, c, r, "lb")

	if got == nil {
		t.Fatal("the cluster's role does not hold the definition")
	}
}

func catalogEntry(name string, spec cloudv1alpha1.ComponentDefinitionSpec) *cloudv1alpha1.ComponentDefinition {
	spec.Description = "The " + name + " component."
	return &cloudv1alpha1.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

const (
	holderClusterID = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"
	holderNamespace = "tenant-acme"
)

// holderCluster is cluster prod of tenant acme, with its ID label.
func holderCluster(edit func(*cloudv1alpha1.Cluster)) *cloudv1alpha1.Cluster {
	c := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: holderNamespace, Name: "prod",
		Labels: map[string]string{cloudstore.LabelTenant: "acme", cloudstore.LabelID: holderClusterID}}}
	if edit != nil {
		edit(c)
	}
	return c
}

// componentRelease is a Helm Release the installer rendered for a component
// of the holder cluster.
func componentRelease(name, comp string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	u.SetNamespace(holderNamespace)
	u.SetName(name)
	u.SetLabels(crossplane.ComponentLabels("acme", holderClusterID, comp))
	return u
}

// deleteAndReconcile deletes the definition and reconciles it once; nil
// when it is gone.
func deleteAndReconcile(t *testing.T, c client.Client, r *ComponentDefinitionReconciler, name string) *cloudv1alpha1.ComponentDefinition {
	t.Helper()
	ctx := t.Context()
	reconcileOnce(t, r, name) // the finalizer
	def := &cloudv1alpha1.ComponentDefinition{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, def); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, def); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, name)
	if err := c.Get(ctx, client.ObjectKey{Name: name}, def); err != nil {
		return nil
	}
	return def
}

// A definition keeps a finalizer, says what uses it, and goes once nothing
// does: a tenant Component holds it; a cluster that still has objects of a
// Platform one holds it until they are gone.
func TestComponentDefinitionInUse(t *testing.T) {
	ctx := t.Context()
	scanner := catalogEntry("scanner", cloudv1alpha1.ComponentDefinitionSpec{})
	lb := catalogEntry("lb", cloudv1alpha1.ComponentDefinitionSpec{Management: cloudv1alpha1.ComponentManagementPlatform})
	use := &cloudv1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Namespace: holderNamespace, Name: "prod-scanner",
		Labels: map[string]string{cloudstore.LabelTenant: "acme"}},
		Spec: cloudv1alpha1.ComponentSpec{ClusterID: holderClusterID, Component: "scanner"}}
	release := componentRelease("prod-lb", "lb")
	c := definitionClient(t, scanner, lb, use, holderCluster(nil), release)
	r := &ComponentDefinitionReconciler{c: c}

	for _, name := range []string{"scanner", "lb"} {
		reconcileOnce(t, r, name)
		got := &cloudv1alpha1.ComponentDefinition{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatal(err)
		}
		if !controllerutil.ContainsFinalizer(got, component.DefinitionFinalizer) {
			t.Fatalf("%s has no finalizer", name)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionDefinitionInUse)
		if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "acme/prod") {
			t.Errorf("%s InUse = %+v", name, cond)
		}
		if err := c.Delete(ctx, got); err != nil {
			t.Fatal(err)
		}
		reconcileOnce(t, r, name)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatalf("%s went while in use: %v", name, err)
		}
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionDefinitionInUse)
		if cond == nil || cond.Reason != "DeletionBlocked" {
			t.Errorf("%s while deleting: %+v", name, cond)
		}
	}

	if err := c.Delete(ctx, use); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, release); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scanner", "lb"} {
		reconcileOnce(t, r, name)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &cloudv1alpha1.ComponentDefinition{}); err == nil {
			t.Errorf("%s is still there once nothing uses it", name)
		}
	}
}

// Whether a cluster holds a Platform definition's delete depends on its
// component's objects in the management cluster, never on what its status
// says: a cluster that stopped reporting does not hold for ever, and one
// that can never release (gone, being deleted) holds nothing.
func TestComponentDefinitionHoldsOnRecordedObjects(t *testing.T) {
	since := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	stale := func(c *cloudv1alpha1.Cluster) {
		c.Status.PlatformComponents = []cloudv1alpha1.PlatformComponentStatus{{Name: "lb", Phase: "Ready"}}
	}
	tests := []struct {
		name    string
		objects []client.Object
		held    bool
		message string
	}{
		{name: "a stale status without objects", objects: []client.Object{holderCluster(stale)}},
		{name: "objects on a running cluster", objects: []client.Object{holderCluster(nil), componentRelease("prod-lb", "lb")},
			held: true, message: "installed on acme/prod"},
		{name: "objects of another component", objects: []client.Object{holderCluster(nil), componentRelease("prod-kyverno", "kyverno")}},
		{name: "objects of a cluster that is gone", objects: []client.Object{componentRelease("prod-lb", "lb")}},
		{name: "objects of a cluster being deleted", objects: []client.Object{
			holderCluster(func(c *cloudv1alpha1.Cluster) { c.Status.Phase = cloudv1alpha1.ClusterPhaseDeleting }),
			componentRelease("prod-lb", "lb")}},
		{name: "objects of a paused cluster", objects: []client.Object{
			holderCluster(nil),
			&cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: "platform-freeze", CreationTimestamp: metav1.NewTime(since)}, Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, By: "alice", Reason: "test"}},
			componentRelease("prod-lb", "lb")},
			held: true, message: "paused by alice since 2026-11-14T09:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lb := catalogEntry("lb", cloudv1alpha1.ComponentDefinitionSpec{Management: cloudv1alpha1.ComponentManagementPlatform})
			c := definitionClient(t, append([]client.Object{lb}, tt.objects...)...)
			r := &ComponentDefinitionReconciler{c: c}

			got := deleteAndReconcile(t, c, r, "lb")

			if (got != nil) != tt.held {
				t.Fatalf("held = %v, want %v", got != nil, tt.held)
			}
			if got != nil {
				cond := meta.FindStatusCondition(got.Status.Conditions, ConditionDefinitionInUse)
				if cond == nil || !strings.Contains(cond.Message, tt.message) {
					t.Errorf("InUse = %+v, want a message naming %q", cond, tt.message)
				}
			}
		})
	}
}

// The force-delete annotation lets a held delete finish, with an event that
// names every holder it ignores; any other value is reported, not obeyed.
func TestComponentDefinitionForceDelete(t *testing.T) {
	tests := []struct {
		name  string
		value string
		gone  bool
	}{
		{name: "true", value: "true", gone: true},
		{name: "not exactly true", value: "yes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lb := catalogEntry("lb", cloudv1alpha1.ComponentDefinitionSpec{Management: cloudv1alpha1.ComponentManagementPlatform})
			lb.Annotations = map[string]string{component.AnnotationForceDelete: tt.value}
			c := definitionClient(t, lb, holderCluster(nil), componentRelease("prod-lb", "lb"))
			recorder := events.NewFakeRecorder(4)
			r := &ComponentDefinitionReconciler{c: c, recorder: recorder}

			got := deleteAndReconcile(t, c, r, "lb")

			if (got == nil) != tt.gone {
				t.Fatalf("gone = %v, want %v", got == nil, tt.gone)
			}
			if tt.gone {
				select {
				case e := <-recorder.Events:
					if !strings.Contains(e, "Warning") || !strings.Contains(e, "acme/prod") {
						t.Errorf("event = %q, want a warning naming the holder", e)
					}
				default:
					t.Error("no event for the forced delete")
				}
				return
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, ConditionDefinitionInUse)
			if cond == nil || !strings.Contains(cond.Message, `"yes"`) {
				t.Errorf("InUse = %+v, want it to say the value is ignored", cond)
			}
		})
	}
}
