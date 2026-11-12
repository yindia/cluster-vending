// Package admin implements the cloud.v1 AdminService: operations of the
// platform's operators on tenants' managed resources.
package admin

import (
	"context"
	"fmt"
	"log/slog"

	"connectrpc.com/connect/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// PausedAnnotation makes Crossplane providers skip a managed resource.
const PausedAnnotation = "crossplane.io/paused"

// Service implements cloud.v1.AdminService.
type Service struct{ deps reconcile.Deps }

var _ cloudv1connect.AdminServiceHandler = (*Service)(nil)

// NewService returns the AdminService.
func NewService(d reconcile.Deps) *Service { return &Service{deps: d} }

// PauseReconcile pauses the tenant's (or one cluster's) managed resources.
func (s *Service) PauseReconcile(ctx context.Context, req *cloudv1.PauseReconcileRequest) (*cloudv1.PauseReconcileResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	n, err := s.setPaused(ctx, req.GetTenantId(), req.GetClusterId(), true)
	return &cloudv1.PauseReconcileResponse{Resources: n}, err
}

// ResumeReconcile resumes the tenant's (or one cluster's) managed
// resources.
func (s *Service) ResumeReconcile(ctx context.Context, req *cloudv1.ResumeReconcileRequest) (*cloudv1.ResumeReconcileResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	n, err := s.setPaused(ctx, req.GetTenantId(), req.GetClusterId(), false)
	return &cloudv1.ResumeReconcileResponse{Resources: n}, err
}

// setPaused sets or removes the paused annotation on every managed
// resource of the tenant (of the cluster, when given) and returns how
// many are in that state now. Objects are read from the API server and
// patched one by one; Usages are not managed resources and are skipped.
// The annotation is the admin's: the platform's applies do not own it,
// so they neither set nor remove it.
func (s *Service) setPaused(ctx context.Context, tenantID, clusterID string, paused bool) (int32, error) {
	selector := client.MatchingLabels{crossplane.LabelTenantID: tenantID}
	if clusterID != "" {
		selector[crossplane.LabelCluster] = clusterID
	}
	var count int32
	for _, obj := range crossplaneaws.ManagedObjects() {
		gvk, err := apiutil.GVKForObject(obj, s.deps.Kube.Scheme())
		if err != nil {
			return count, connect.NewError(connect.CodeInternal, err.Error())
		}
		if gvk.Kind == "Usage" {
			continue
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		err = s.deps.Reader().List(ctx, list, client.InNamespace(kube.TenantNamespace(tenantID)), selector)
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return count, connect.NewError(connect.CodeUnavailable, fmt.Sprintf("cannot list %s: %v", gvk.Kind, err))
		}
		for i := range list.Items {
			if err := s.patch(ctx, &list.Items[i], paused); err != nil {
				return count, connect.NewError(connect.CodeInternal,
					fmt.Sprintf("%s %s: %v (%d resources done; the call can be repeated)", gvk.Kind, list.Items[i].GetName(), err, count))
			}
			count++
		}
	}
	s.logger().InfoContext(ctx, "reconciliation paused state set", "tenant_id", tenantID, "cluster_id", clusterID,
		"paused", paused, "resources", count)
	return count, nil
}

func (s *Service) patch(ctx context.Context, u *unstructured.Unstructured, paused bool) error {
	_, has := u.GetAnnotations()[PausedAnnotation]
	if has == paused {
		return nil
	}
	before := client.MergeFrom(u.DeepCopy())
	annotations := u.GetAnnotations()
	if paused {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[PausedAnnotation] = "true"
	} else {
		delete(annotations, PausedAnnotation)
	}
	u.SetAnnotations(annotations)
	return client.IgnoreNotFound(s.deps.Kube.Patch(ctx, u, before))
}

func (s *Service) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}
