package tenant

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	tenantreconcile "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

var directoryTenantID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,54}[a-z0-9])?$`)

func directoryCanCreate(id *auth.Identity) bool {
	return id.Anonymous || id.IsPlatformAdmin() || slices.ContainsFunc(id.Grants, func(grant auth.Grant) bool {
		return grant.Role == auth.RoleTenantAdmin
	})
}

func directoryInfo(id string, created time.Time) *v1.TenantInfo {
	out := &v1.TenantInfo{Id: id}
	if !created.IsZero() {
		out.CreateTime = timestamppb.New(created)
	}
	return out
}

func (s *TenantService) directoryReader() client.Reader {
	if s.deps.KubeReader != nil {
		return s.deps.KubeReader
	}
	return s.deps.Kube
}

// ListTenants is open to accepted callers, but visibility is checked for EACH
// registered tenant. Neither grants for absent tenants nor unrelated namespaces
// become directory entries. No account, provider, annotation or grant is returned.
func (s *TenantService) ListTenants(ctx context.Context, req *v1.ListTenantsRequest) (*v1.ListTenantsResponse, error) {
	id := auth.FromContext(ctx)
	if err := id.Authorize(auth.Access{Scope: auth.ScopeAuthenticated}, ""); err != nil {
		return nil, service.ToConnect(err)
	}
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	reader := s.directoryReader()
	if reader == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "management cluster is unavailable")
	}
	list := &cloudv1alpha1.TenantList{}
	if err := reader.List(ctx, list); err != nil {
		s.logger().ErrorContext(ctx, "list registered tenants", "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the tenant directory")
	}
	out := &v1.ListTenantsResponse{CanCreateTenants: directoryCanCreate(id)}
	for i := range list.Items {
		registration := &list.Items[i]
		tenant := registration.Name
		if registration.DeletionTimestamp != nil || !directoryTenantID.MatchString(tenant) {
			continue
		}
		if err := id.Authorize(auth.Access{Scope: auth.ScopeTenant}, tenant); err != nil {
			continue
		}
		// A deleting namespace cannot serve a tenant even before its CR is removed.
		namespace := &corev1.Namespace{}
		if err := reader.Get(ctx, client.ObjectKey{Name: kube.TenantNamespace(tenant)}, namespace); err == nil {
			if namespace.DeletionTimestamp != nil || namespace.Status.Phase == corev1.NamespaceTerminating {
				continue
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeUnavailable, "cannot read tenant namespace availability")
		}
		out.Tenants = append(out.Tenants, directoryInfo(tenant, tenantreconcile.Timestamp(registration)))
	}
	slices.SortFunc(out.Tenants, func(a, b *v1.TenantInfo) int {
		if a.Id < b.Id {
			return -1
		}
		if a.Id > b.Id {
			return 1
		}
		return 0
	})
	return out, nil
}

// CreateTenant registers the cluster-scoped Tenant through the canonical store.
// The shared lifecycle helper exclusively creates/validates its namespace;
// unmarked and foreign namespace collisions are never adopted.
func (s *TenantService) CreateTenant(ctx context.Context, req *v1.CreateTenantRequest) (*v1.CreateTenantResponse, error) {
	if err := auth.FromContext(ctx).Authorize(auth.Access{Scope: auth.ScopeTenant, Write: true}, req.GetTenantId()); err != nil {
		return nil, service.ToConnect(err)
	}
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if s.deps.Kube == nil || s.deps.Store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "tenant registration is unavailable")
	}
	id := req.GetTenantId()
	// Direct client avoids accepting stale cached ownership after a create race.
	namespace := &corev1.Namespace{}
	if err := s.deps.Kube.Get(ctx, client.ObjectKey{Name: kube.TenantNamespace(id)}, namespace); err == nil {
		if namespace.Labels[cloudstore.LabelTenant] != id && (namespace.Labels[cloudstore.LabelTenant] != "" || namespace.Annotations[tenantreconcile.AnnotationRetainedTenant] != id) {
			return nil, connect.NewError(connect.CodeAlreadyExists, "the namespace exists and cannot be adopted by this tenant")
		}
		if namespace.DeletionTimestamp != nil || namespace.Status.Phase == corev1.NamespaceTerminating {
			return nil, connect.NewError(connect.CodeFailedPrecondition, "tenant namespace is being deleted")
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read tenant registration")
	}
	if err := s.deps.Store.Tenants().Create(ctx, &model.Tenant{ID: id}); err != nil && !errors.Is(err, repository.ErrAlreadyExists) {
		if errors.Is(err, tenantreconcile.ErrNamespaceCollision) || errors.Is(err, repository.ErrConflict) {
			return nil, connect.NewError(connect.CodeAlreadyExists, "tenant namespace exists and cannot be adopted")
		}
		return nil, service.ToConnect(err)
	}
	registration := &cloudv1alpha1.Tenant{}
	if err := s.deps.Kube.Get(ctx, client.ObjectKey{Name: id}, registration); err != nil {
		return nil, connect.NewError(connect.CodeAborted, "tenant registration changed concurrently; refresh and try again")
	}
	if registration.DeletionTimestamp != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "tenant is being deleted")
	}
	if _, err := tenantreconcile.EnsureNamespace(ctx, s.deps.Kube, registration); err != nil {
		if errors.Is(err, tenantreconcile.ErrNamespaceCollision) || errors.Is(err, repository.ErrConflict) {
			return nil, connect.NewError(connect.CodeAlreadyExists, "tenant namespace exists and cannot be adopted")
		}
		return nil, service.ToConnect(err)
	}
	row, err := s.deps.Store.Tenants().Get(ctx, id)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	if row == nil || row.ID != id {
		return nil, connect.NewError(connect.CodeAborted, "tenant registration changed concurrently; refresh and try again")
	}
	created := row.CreatedAt
	if created.IsZero() {
		created = registration.CreationTimestamp.Time
	}
	return &v1.CreateTenantResponse{Tenant: directoryInfo(row.ID, created)}, nil
}
