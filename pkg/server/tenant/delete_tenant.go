package tenant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	tenantreconcile "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// maxNamedDependents is how many objects of one kind a refused DeleteTenant
// names; the count covers the rest.
const maxNamedDependents = 5

// dependentKind is a kind of object in the tenant namespace that keeps the
// tenant from being deleted, in the order the refusal names them.
type dependentKind struct {
	plural string
	list   func() client.ObjectList
}

var tenantDependents = []dependentKind{
	{"sub-tenants", func() client.ObjectList { return &cloudv1alpha1.SubTenantList{} }},
	{"components", func() client.ObjectList { return &cloudv1alpha1.ComponentList{} }},
	{"clusters", func() client.ObjectList { return &cloudv1alpha1.ClusterList{} }},
	{"networks", func() client.ObjectList { return &cloudv1alpha1.NetworkList{} }},
	{"cloud accounts", func() client.ObjectList { return &cloudv1alpha1.CloudAccountList{} }},
}

// DeleteTenant deletes the tenant's registration once nothing is left in
// its namespace. The Tenant's finalizer (the tenant controller) unregisters
// the namespace, keeping it and its contents; a registration the
// controller has not finalized yet is unregistered here the same way.
func (s *TenantService) DeleteTenant(ctx context.Context, req *v1.DeleteTenantRequest) (*v1.DeleteTenantResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if s.deps.Kube == nil || s.deps.Store == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "tenant registration is unavailable")
	}
	id := req.GetTenantId()
	registration := &cloudv1alpha1.Tenant{}
	err := s.deps.Kube.Get(ctx, client.ObjectKey{Name: id}, registration)
	canonical := err == nil
	switch {
	case canonical && registration.DeletionTimestamp != nil:
		// Already asked for: the controller finishes it.
		return &v1.DeleteTenantResponse{Tenant: directoryInfo(id, tenantreconcile.Timestamp(registration))}, nil
	case err != nil && !apierrors.IsNotFound(err):
		return nil, s.unavailable(ctx, "read tenant registration", id, err)
	}
	row, err := s.deps.Store.Tenants().Get(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Sprintf("tenant %s is not registered", id))
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	remaining, err := s.tenantDependents(ctx, id)
	if err != nil {
		return nil, s.unavailable(ctx, "list the tenant's objects", id, err)
	}
	if len(remaining) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"tenant %s still has %s; delete them first (sub-tenants, components, clusters, networks, then accounts)",
			id, strings.Join(remaining, ", ")))
	}
	info := directoryInfo(id, row.CreatedAt)
	if canonical {
		if err := s.deleteRegistration(ctx, registration); err != nil {
			return nil, err
		}
	} else if err := tenantreconcile.RetainNamespace(ctx, s.deps.Kube, id); err != nil {
		// A legacy tenant: its namespace is its only registration.
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "tenant deleted", "tenant_id", id, "namespace", kube.TenantNamespace(id))
	return &v1.DeleteTenantResponse{Tenant: info}, nil
}

// deleteRegistration deletes the Tenant CR as it was read. With the
// controller's finalizer the controller unregisters the namespace; without
// one (the controller has not seen it yet) the namespace is unregistered
// first, since nothing else would.
func (s *TenantService) deleteRegistration(ctx context.Context, registration *cloudv1alpha1.Tenant) error {
	id := registration.Name
	if !controllerutil.ContainsFinalizer(registration, cloudv1alpha1.TenantFinalizer) {
		if err := tenantreconcile.RetainNamespace(ctx, s.deps.Kube, id); err != nil {
			return service.ToConnect(err)
		}
	}
	uid, version := registration.UID, registration.ResourceVersion
	err := s.deps.Kube.Delete(ctx, registration, client.PropagationPolicy(metav1.DeletePropagationOrphan),
		client.Preconditions{UID: &uid, ResourceVersion: &version})
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return nil
	case apierrors.IsConflict(err):
		return connect.NewError(connect.CodeAborted, "tenant registration changed concurrently; refresh and try again")
	default:
		return s.unavailable(ctx, "delete tenant registration", id, err)
	}
}

// tenantDependents describes what is left in the tenant's namespace, e.g.
// "2 clusters (a, b)"; empty when nothing is. Objects being deleted count:
// they are not gone yet.
func (s *TenantService) tenantDependents(ctx context.Context, id string) ([]string, error) {
	var out []string
	for _, kind := range tenantDependents {
		list := kind.list()
		if err := s.deps.Kube.List(ctx, list, client.InNamespace(kube.TenantNamespace(id))); err != nil {
			if meta.IsNoMatchError(err) {
				continue // the kind is not installed: there is none
			}
			return nil, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			continue
		}
		names := make([]string, 0, len(items))
		for _, item := range items {
			if o, ok := item.(client.Object); ok {
				names = append(names, o.GetName())
			}
		}
		slices.Sort(names)
		out = append(out, describeDependents(len(items), kind.plural, names))
	}
	return out, nil
}

// describeDependents is "1 cluster (a)" or "7 networks (a, b, c, d, e, ...)".
func describeDependents(n int, plural string, names []string) string {
	noun := plural
	if n == 1 {
		noun = strings.TrimSuffix(plural, "s")
	}
	shown := names[:min(len(names), maxNamedDependents)]
	more := ""
	if len(names) > len(shown) {
		more = ", ..."
	}
	return fmt.Sprintf("%d %s (%s%s)", n, noun, strings.Join(shown, ", "), more)
}

func (s *TenantService) unavailable(ctx context.Context, what, id string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return service.ToConnect(err)
	}
	s.logger().ErrorContext(ctx, what, "tenant_id", id, "error", err)
	return connect.NewError(connect.CodeUnavailable, "cannot "+what)
}
