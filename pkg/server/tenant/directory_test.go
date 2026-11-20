package tenant

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	tenantreconcile "github.com/yindia/cluster-vending/pkg/reconcile/tenant"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

var directoryCreatedAt = time.Date(2026, 11, 16, 1, 2, 3, 456789123, time.UTC)

func TestDirectoryListReadErrors(t *testing.T) {
	for _, stage := range []string{"list", "namespace"} {
		for _, test := range []struct {
			name         string
			err          error
			code         connect.Code
			wantErrorLog bool
		}{
			{"canceled", context.Canceled, connect.CodeCanceled, false},
			{"wrapped canceled", fmt.Errorf("read interrupted: %w", context.Canceled), connect.CodeCanceled, false},
			{"deadline", context.DeadlineExceeded, connect.CodeDeadlineExceeded, false},
			{"wrapped deadline", fmt.Errorf("read interrupted: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded, false},
			{"backend failure", errors.New("management transport failed"), connect.CodeUnavailable, true},
		} {
			t.Run(stage+"/"+test.name, func(t *testing.T) {
				s, c := directoryWorld(t, directoryRegistration("acme"), directoryNamespace("acme"))
				var logs bytes.Buffer
				s.deps.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
				s.deps.KubeReader = interceptor.NewClient(c, interceptor.Funcs{
					List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if stage == "list" {
							return test.err
						}
						return underlying.List(ctx, list, opts...)
					},
					Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if stage == "namespace" {
							return test.err
						}
						return underlying.Get(ctx, key, obj, opts...)
					},
				})
				ctx := directoryContext(t, auth.Grant{Role: auth.RoleTenantViewer, Tenants: []string{"acme"}})
				out, err := s.ListTenants(ctx, &v1.ListTenantsRequest{})
				if out != nil || connect.CodeOf(err) != test.code {
					t.Fatalf("ListTenants = %v, %v; want %v", out, err, test.code)
				}
				if got := strings.Contains(logs.String(), `"level":"ERROR"`); got != test.wantErrorLog {
					t.Fatalf("ERROR log = %v, want %v: %s", got, test.wantErrorLog, logs.String())
				}
				if test.wantErrorLog && (!strings.Contains(logs.String(), test.err.Error()) || strings.Contains(err.Error(), test.err.Error())) {
					t.Fatalf("backend error must be logged, not exposed: log=%s err=%v", logs.String(), err)
				}
			})
		}
	}
}

func directoryNamespace(id string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: kube.TenantNamespace(id),
		Labels:      map[string]string{cloudstore.LabelTenant: id},
		Annotations: map[string]string{cloudstore.AnnotationCreatedAt: directoryCreatedAt.Format(time.RFC3339Nano), cloudstore.AnnotationDisplayName: "private display " + id},
	}}
}

func directoryRegistration(id string) *cloudv1alpha1.Tenant {
	return &cloudv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: id, Annotations: map[string]string{cloudstore.AnnotationCreatedAt: directoryCreatedAt.Format(time.RFC3339Nano)}}, Spec: cloudv1alpha1.TenantSpec{DisplayName: "private display " + id}}
}

func directoryWorld(t *testing.T, objects ...client.Object) (*TenantService, client.WithWatch) {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return NewTenantService(reconcile.Deps{Kube: c, Store: cloudstore.New(c)}), c
}

func directoryContext(t *testing.T, grants ...auth.Grant) context.Context {
	t.Helper()
	return auth.WithIdentity(t.Context(), &auth.Identity{Subject: "directory-user", Grants: grants})
}

func TestDirectoryListVisibilityAndCreationCapability(t *testing.T) {
	for _, test := range []struct {
		name      string
		identity  *auth.Identity
		want      []string
		canCreate bool
	}{
		{"viewer", &auth.Identity{Grants: []auth.Grant{{Role: auth.RoleTenantViewer, Tenants: []string{"acme"}}}}, []string{"acme"}, false},
		{"exact admin", &auth.Identity{Grants: []auth.Grant{{Role: auth.RoleTenantAdmin, Tenants: []string{"acme", "absent"}}}}, []string{"acme"}, true},
		{"wildcard viewer", &auth.Identity{Grants: []auth.Grant{{Role: auth.RoleTenantViewer, Tenants: []string{"*"}}}}, []string{"acme", "foreign"}, false},
		{"wildcard admin", &auth.Identity{Grants: []auth.Grant{{Role: auth.RoleTenantAdmin, Tenants: []string{"*"}}}}, []string{"acme", "foreign"}, true},
		{"platform admin", &auth.Identity{Grants: []auth.Grant{{Role: auth.RolePlatformAdmin}}}, []string{"acme", "foreign"}, true},
		{"anonymous", auth.Anonymous(), []string{"acme", "foreign"}, true},
		{"no grants", &auth.Identity{Subject: "no-grants"}, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			mislabeled := directoryNamespace("acme")
			mislabeled.Name = "unrelated-namespace"
			s, _ := directoryWorld(t, directoryRegistration("foreign"), directoryRegistration("acme"), directoryNamespace("foreign"), directoryNamespace("acme"), mislabeled,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-unmarked"}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-empty", Labels: map[string]string{cloudstore.LabelTenant: ""}}},
			)
			out, err := s.ListTenants(auth.WithIdentity(t.Context(), test.identity), &v1.ListTenantsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, tenant := range out.Tenants {
				ids = append(ids, tenant.Id)
				if !tenant.GetCreateTime().AsTime().Equal(directoryCreatedAt) {
					t.Fatalf("creation timestamp lost precision: %v", tenant)
				}
			}
			if !slices.Equal(ids, test.want) || out.CanCreateTenants != test.canCreate {
				t.Fatalf("list = %v, want IDs %v and create=%v", out, test.want, test.canCreate)
			}
			raw, _ := protojson.Marshal(out)
			if strings.Contains(string(raw), "private display") || (len(test.want) <= 1 && strings.Contains(string(raw), "foreign")) {
				t.Fatalf("directory leaked metadata: %s", raw)
			}
		})
	}
}

func TestDirectoryCreateRegistersTenantCRAndNamespaceAndReturnsCanonical(t *testing.T) {
	s, c := directoryWorld(t)
	ctx := directoryContext(t, auth.Grant{Role: auth.RoleTenantAdmin, Tenants: []string{"new-tenant"}})
	before := time.Now()
	first, err := s.CreateTenant(ctx, &v1.CreateTenantRequest{TenantId: "new-tenant"})
	if err != nil {
		t.Fatal(err)
	}
	if first.GetTenant().GetId() != "new-tenant" || first.Tenant.GetCreateTime() == nil || first.Tenant.CreateTime.AsTime().Before(before) {
		t.Fatalf("create = %v", first)
	}
	namespace := &corev1.Namespace{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "tenant-new-tenant"}, namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels[cloudstore.LabelTenant] != "new-tenant" || namespace.Annotations[cloudstore.AnnotationCreatedAt] != first.Tenant.CreateTime.AsTime().Format(time.RFC3339Nano) {
		t.Fatalf("canonical tenant marker: %v", namespace)
	}
	beforeNamespace := namespace.DeepCopy()
	second, err := s.CreateTenant(ctx, &v1.CreateTenantRequest{TenantId: "new-tenant"})
	if err != nil || !proto.Equal(first, second) {
		t.Fatalf("retry not canonical: %v %v", second, err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(namespace), namespace); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeNamespace, namespace) {
		t.Fatal("retry rewrote namespace metadata")
	}
	list, err := s.ListTenants(ctx, &v1.ListTenantsRequest{})
	if err != nil || len(list.GetTenants()) != 1 || !proto.Equal(list.Tenants[0], first.Tenant) {
		t.Fatalf("list/create disagree: %v %v", list, err)
	}
	registrations := &cloudv1alpha1.TenantList{}
	if err := c.List(t.Context(), registrations); err != nil || len(registrations.Items) != 1 {
		t.Fatalf("Tenant CR registration missing: %v %v", registrations, err)
	}
	accounts := &cloudv1alpha1.CloudAccountList{}
	if err := c.List(t.Context(), accounts); err != nil || len(accounts.Items) != 0 {
		t.Fatalf("account unexpectedly created: %v %v", accounts, err)
	}
}

func TestDirectoryCreatePermissionsAndValidation(t *testing.T) {
	for _, test := range []struct {
		name, target string
		identity     *auth.Identity
		want         connect.Code
	}{
		{"viewer denied", "acme", &auth.Identity{Grants: []auth.Grant{{Role: auth.RoleTenantViewer, Tenants: []string{"acme"}}}}, connect.CodePermissionDenied},
		{"exact admin wrong target", "foreign", &auth.Identity{Grants: []auth.Grant{{Role: auth.RoleTenantAdmin, Tenants: []string{"acme"}}}}, connect.CodePermissionDenied},
		{"exact admin own", "acme", &auth.Identity{Grants: []auth.Grant{{Role: auth.RoleTenantAdmin, Tenants: []string{"acme"}}}}, 0},
		{"wildcard admin", "new-team", &auth.Identity{Grants: []auth.Grant{{Role: auth.RoleTenantAdmin, Tenants: []string{"*"}}}}, 0},
		{"platform admin", "new-team", &auth.Identity{Grants: []auth.Grant{{Role: auth.RolePlatformAdmin}}}, 0},
		{"anonymous", "new-team", auth.Anonymous(), 0},
		{"empty ID", "", auth.Anonymous(), connect.CodeInvalidArgument},
		{"uppercase ID", "Bad_Tenant", auth.Anonymous(), connect.CodeInvalidArgument},
		{"trailing hyphen", "bad-", auth.Anonymous(), connect.CodeInvalidArgument},
		{"too long", strings.Repeat("a", 57), auth.Anonymous(), connect.CodeInvalidArgument},
		{"maximum length", strings.Repeat("a", 56), auth.Anonymous(), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, c := directoryWorld(t)
			_, err := s.CreateTenant(auth.WithIdentity(t.Context(), test.identity), &v1.CreateTenantRequest{TenantId: test.target})
			if (test.want == 0 && err != nil) || (test.want != 0 && (err == nil || connect.CodeOf(err) != test.want)) {
				t.Fatalf("create = %v, want %v", err, test.want)
			}
			all := &corev1.NamespaceList{}
			if err := c.List(t.Context(), all); err != nil {
				t.Fatal(err)
			}
			if (len(all.Items) == 0) != (test.want != 0) {
				t.Fatal("denied/invalid create changed namespace inventory")
			}
		})
	}
}

func TestDirectoryNeverAdoptsNamespaceCollisions(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"unmarked":       nil,
		"empty marker":   {cloudstore.LabelTenant: ""},
		"foreign marker": {cloudstore.LabelTenant: "foreign-private"},
	} {
		t.Run(name, func(t *testing.T) {
			original := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: labels, Annotations: map[string]string{"private": "leave me alone"}}}
			s, c := directoryWorld(t, original)
			stored := &corev1.Namespace{}
			if err := c.Get(t.Context(), client.ObjectKey{Name: original.Name}, stored); err != nil {
				t.Fatal(err)
			}
			before := stored.DeepCopy()
			_, err := s.CreateTenant(directoryContext(t, auth.Grant{Role: auth.RoleTenantAdmin, Tenants: []string{"acme"}}), &v1.CreateTenantRequest{TenantId: "acme"})
			if connect.CodeOf(err) != connect.CodeAlreadyExists || strings.Contains(err.Error(), "foreign-private") {
				t.Fatalf("collision = %v", err)
			}
			if err := c.Get(t.Context(), client.ObjectKey{Name: original.Name}, stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, stored) {
				t.Fatal("collision namespace was adopted or mutated")
			}
		})
	}
}

func TestDirectoryConcurrentUnmarkedNamespaceIsNotAdopted(t *testing.T) {
	s, c := directoryWorld(t)
	var inserted *corev1.Namespace
	wrapped := interceptor.NewClient(c, interceptor.Funcs{Create: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*corev1.Namespace); ok {
			inserted = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: obj.GetName(), Annotations: map[string]string{"private": "concurrent owner"}}}
			if err := underlying.Create(ctx, inserted); err != nil {
				return err
			}
		}
		return underlying.Create(ctx, obj, opts...)
	}})
	s.deps.Kube = wrapped
	s.deps.Store = cloudstore.New(wrapped)
	_, err := s.CreateTenant(t.Context(), &v1.CreateTenantRequest{TenantId: "raced"})
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("concurrent collision = %v", err)
	}
	after := &corev1.Namespace{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "tenant-raced"}, after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inserted, after) || after.Labels[cloudstore.LabelTenant] != "" {
		t.Fatal("concurrent namespace was adopted")
	}
}

func TestDirectoryConcurrentMarkedTenantReturnsCanonical(t *testing.T) {
	s, _ := directoryWorld(t)
	const callers = 8
	var wait sync.WaitGroup
	responses := make(chan *v1.CreateTenantResponse, callers)
	errors := make(chan error, callers)
	for range callers {
		wait.Go(func() {
			result, err := s.CreateTenant(t.Context(), &v1.CreateTenantRequest{TenantId: "concurrent"})
			responses <- result
			errors <- err
		})
	}
	wait.Wait()
	close(responses)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent create failed: %v", err)
		}
	}
	var first *v1.CreateTenantResponse
	for response := range responses {
		if first == nil {
			first = response
		}
		if !proto.Equal(first, response) {
			t.Fatal("concurrent callers received different canonical tenants")
		}
	}
}

func TestDirectoryLegacyTimestampAndNoManagementClient(t *testing.T) {
	legacy := directoryNamespace("legacy")
	legacy.Annotations = nil
	legacy.CreationTimestamp = metav1.NewTime(directoryCreatedAt)
	s, _ := directoryWorld(t, legacy, directoryRegistration("legacy"))
	created, err := s.CreateTenant(t.Context(), &v1.CreateTenantRequest{TenantId: "legacy"})
	if err != nil || !created.GetTenant().GetCreateTime().AsTime().Equal(directoryCreatedAt) {
		t.Fatalf("legacy create time = %v %v", created, err)
	}
	listed, err := s.ListTenants(t.Context(), &v1.ListTenantsRequest{})
	if err != nil || !proto.Equal(listed.GetTenants()[0], created.Tenant) {
		t.Fatalf("legacy list time = %v %v", listed, err)
	}
	nilService := NewTenantService(reconcile.Deps{})
	if _, err := nilService.CreateTenant(t.Context(), &v1.CreateTenantRequest{TenantId: "new"}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatal(err)
	}
	if _, err := nilService.ListTenants(t.Context(), &v1.ListTenantsRequest{}); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatal(err)
	}
}

func TestDirectoryExcludesTerminatingNamespaces(t *testing.T) {
	deleting := directoryNamespace("deleting")
	deletedAt := metav1.Now()
	deleting.DeletionTimestamp = &deletedAt
	deleting.Finalizers = []string{"test.example/cleanup"}
	terminating := directoryNamespace("terminating")
	terminating.Status.Phase = corev1.NamespaceTerminating
	s, _ := directoryWorld(t, directoryRegistration("ready"), directoryRegistration("deleting"), directoryRegistration("terminating"), directoryNamespace("ready"), deleting, terminating)
	list, err := s.ListTenants(t.Context(), &v1.ListTenantsRequest{})
	if err != nil || len(list.GetTenants()) != 1 || list.Tenants[0].Id != "ready" {
		t.Fatalf("terminating namespace listed: %v %v", list, err)
	}
	for _, id := range []string{"deleting", "terminating"} {
		if _, err := s.CreateTenant(t.Context(), &v1.CreateTenantRequest{TenantId: id}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("creation into terminating namespace %q: %v", id, err)
		}
	}
}

func TestDirectoryLegacyNamespaceAloneNotListed(t *testing.T) {
	s, _ := directoryWorld(t, directoryNamespace("legacy"))
	out, err := s.ListTenants(t.Context(), &v1.ListTenantsRequest{})
	if err != nil || len(out.GetTenants()) != 0 {
		t.Fatalf("namespace-only tenant listed: %v %v", out, err)
	}
}
func TestDirectoryRetainedNamespaceCanBeRegisteredAgain(t *testing.T) {
	for _, marker := range []string{"acme", "foreign"} {
		t.Run(marker, func(t *testing.T) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Annotations: map[string]string{tenantreconcile.AnnotationRetainedTenant: marker}}}
			s, c := directoryWorld(t, ns)
			out, err := s.CreateTenant(t.Context(), &v1.CreateTenantRequest{TenantId: "acme"})
			if marker != "acme" {
				if connect.CodeOf(err) != connect.CodeAlreadyExists {
					t.Fatalf("foreign retained marker: %v", err)
				}
				return
			}
			if err != nil || out.GetTenant().GetId() != "acme" {
				t.Fatalf("recreate: %v %v", out, err)
			}
			if err := c.Get(t.Context(), client.ObjectKey{Name: "tenant-acme"}, ns); err != nil {
				t.Fatal(err)
			}
			if ns.Labels[cloudstore.LabelTenant] != "acme" || ns.Annotations[tenantreconcile.AnnotationRetainedTenant] != "" {
				t.Fatalf("namespace not safely reclaimed: %v", ns)
			}
		})
	}
}

func TestDirectoryDeleteThenRecreateRetainsNamespaceContents(t *testing.T) {
	s, c := directoryWorld(t)
	req := &v1.CreateTenantRequest{TenantId: "acme"}
	if _, err := s.CreateTenant(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	content := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "retained-content", Namespace: "tenant-acme"}, Data: map[string]string{"keep": "value"}}
	if err := c.Create(t.Context(), content); err != nil {
		t.Fatal(err)
	}
	if err := s.deps.Store.Tenants().Delete(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	out, err := s.CreateTenant(t.Context(), req)
	if err != nil || out.GetTenant().GetId() != "acme" {
		t.Fatalf("recreate: %v %v", out, err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(content), content); err != nil || content.Data["keep"] != "value" {
		t.Fatalf("retained data changed: %v %v", content, err)
	}
}
