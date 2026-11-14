// Package admin implements the cloud.v1 AdminService: operations of the
// platform's operators on tenants' managed resources.
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// PausedAnnotation makes Crossplane providers skip a managed resource.
const PausedAnnotation = managed.PausedAnnotation

// Service implements cloud.v1.AdminService.
type Service struct {
	deps reconcile.Deps
	// now is the clock; tests replace it.
	now func() time.Time
}

var _ cloudv1connect.AdminServiceHandler = (*Service)(nil)

// NewService returns the AdminService.
func NewService(d reconcile.Deps) *Service { return &Service{deps: d, now: time.Now} }

// PauseReconcile pauses the tenant, or one of its clusters. The pause is
// recorded on the rows first (the tenant's accounts, networks and
// clusters, or the one cluster: Tenants.SetPause), with who asked, when
// and why, so the controller stops acting on them; then every managed
// resource of the scope gets the providers' pause.
func (s *Service) PauseReconcile(ctx context.Context, req *cloudv1.PauseReconcileRequest) (*cloudv1.PauseReconcileResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	target := model.PauseTarget{NetworkID: req.GetNetworkId(), ClusterID: req.GetClusterId()}
	pause := &model.Pause{By: caller(ctx), Since: s.now().UTC(), Reason: req.GetReason()}
	if err := s.deps.Store.Tenants().SetPause(ctx, req.GetTenantId(), target, pause); err != nil {
		return nil, s.storeError(ctx, "pause", req.GetTenantId(), target, err)
	}
	n, err := s.setPaused(ctx, req.GetTenantId(), target, true)
	return &cloudv1.PauseReconcileResponse{Resources: n}, err
}

// ResumeReconcile resumes the tenant, or one of its clusters: the pause
// recorded on the rows is removed, then the providers' pause on the
// managed resources. The controller's next pass observes each resource
// and applies what was asked for meanwhile.
func (s *Service) ResumeReconcile(ctx context.Context, req *cloudv1.ResumeReconcileRequest) (*cloudv1.ResumeReconcileResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	target := model.PauseTarget{NetworkID: req.GetNetworkId(), ClusterID: req.GetClusterId()}
	if err := s.deps.Store.Tenants().SetPause(ctx, req.GetTenantId(), target, nil); err != nil {
		return nil, s.storeError(ctx, "resume", req.GetTenantId(), target, err)
	}
	n, err := s.setPaused(ctx, req.GetTenantId(), target, false)
	return &cloudv1.ResumeReconcileResponse{Resources: n}, err
}

// anonymousCaller is who paused when the API runs without authentication.
const anonymousCaller = "anonymous"

// caller names who makes the request, for the pause's record: the
// caller's username, else its subject; "anonymous" without a token.
func caller(ctx context.Context) string {
	id := auth.FromContext(ctx)
	switch {
	case id == nil || id.Anonymous:
		return anonymousCaller
	case id.Username != "":
		return id.Username
	case id.Subject != "":
		return id.Subject
	default:
		return anonymousCaller
	}
}

// storeError answers a failed record of a pause: NOT_FOUND for a tenant or
// a cluster that does not exist, else what the store's failures map to.
func (s *Service) storeError(ctx context.Context, verb, tenantID string, target model.PauseTarget, err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		switch {
		case target.ClusterID != "":
			return connect.NewError(connect.CodeNotFound, fmt.Sprintf("cluster %s of tenant %s not found", target.ClusterID, tenantID))
		case target.NetworkID != "":
			return connect.NewError(connect.CodeNotFound, fmt.Sprintf("network %s of tenant %s not found", target.NetworkID, tenantID))
		}
		return connect.NewError(connect.CodeNotFound, fmt.Sprintf("tenant %s not found", tenantID))
	}
	s.logger().ErrorContext(ctx, "record the "+verb, "tenant_id", tenantID, "cluster_id", target.ClusterID,
		"network_id", target.NetworkID, "error", err)
	return service.ToConnect(err)
}

// setPaused sets or removes the paused annotation on every managed
// resource of the target (the tenant's, or one network's or cluster's)
// and returns how many are in that state now. Objects are read from the
// API server and patched one by one (pausedKinds). A tenant's resume
// leaves the annotation on the resources of a network or cluster that is
// still paused on its own (stillPaused).
// The annotation is the admin's: the platform's applies do not own it,
// so they neither set nor remove it.
func (s *Service) setPaused(ctx context.Context, tenantID string, target model.PauseTarget, paused bool) (int32, error) {
	selector := client.MatchingLabels{crossplane.LabelTenantID: tenantID}
	switch {
	case target.ClusterID != "":
		selector[crossplane.LabelCluster] = target.ClusterID
	case target.NetworkID != "":
		selector[crossplane.LabelNetwork] = target.NetworkID
	}
	keep := stillPaused{s: s, tenantID: tenantID, rows: map[string]bool{}}
	if paused || target.Scope() != model.PauseTenant {
		keep.s = nil // pausing, or resuming one network or cluster: every resource of it
	}
	kinds, err := s.pausedKinds()
	if err != nil {
		return 0, connect.NewError(connect.CodeInternal, err.Error())
	}
	var count int32
	for _, gvk := range kinds {
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
			stays, err := keep.holds(ctx, &list.Items[i])
			if err != nil {
				return count, connect.NewError(connect.CodeUnavailable, err.Error())
			}
			if stays {
				continue
			}
			if err := s.patch(ctx, &list.Items[i], paused); err != nil {
				return count, connect.NewError(connect.CodeInternal,
					fmt.Sprintf("%s %s: %v (%d resources done; the call can be repeated)", gvk.Kind, list.Items[i].GetName(), err, count))
			}
			count++
		}
	}
	s.logger().InfoContext(ctx, "reconciliation paused state set", "tenant_id", tenantID, "cluster_id", target.ClusterID,
		"network_id", target.NetworkID, "paused", paused, "resources", count)
	return count, nil
}

// pausedKinds are the kinds a pause covers: every managed resource kind of
// the AWS providers the platform renders, and provider-helm's Release (a
// platform component's chart). Usages are not managed resources; nor are
// ProviderConfigs.
func (s *Service) pausedKinds() ([]schema.GroupVersionKind, error) {
	objects := crossplaneaws.ManagedObjects()
	out := make([]schema.GroupVersionKind, 0, len(objects)+1)
	for _, obj := range objects {
		gvk, err := apiutil.GVKForObject(obj, s.deps.Kube.Scheme())
		if err != nil {
			return nil, err
		}
		if gvk.Kind != "Usage" {
			out = append(out, gvk)
		}
	}
	return append(out, crossplaneaws.HelmReleaseGVK), nil
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

// stillPaused tells a tenant's resume which managed resources keep the
// providers' pause: those of a network or cluster that is still paused on
// its own (its row's pause, read once per row). A nil s keeps none.
type stillPaused struct {
	s        *Service
	tenantID string
	rows     map[string]bool
}

// holds reports whether u belongs to a network or cluster still paused.
func (k stillPaused) holds(ctx context.Context, u *unstructured.Unstructured) (bool, error) {
	if k.s == nil {
		return false, nil
	}
	labels := u.GetLabels()
	if id := labels[crossplane.LabelCluster]; id != "" {
		return k.row(ctx, "cluster/"+id, func() (*model.Pause, error) {
			c, err := k.s.deps.Store.Clusters().Get(ctx, k.tenantID, id)
			if err != nil {
				return nil, err
			}
			return c.Paused, nil
		})
	}
	if id := labels[crossplane.LabelNetwork]; id != "" {
		return k.row(ctx, "network/"+id, func() (*model.Pause, error) {
			n, err := k.s.deps.Store.Networks().Get(ctx, k.tenantID, id)
			if err != nil {
				return nil, err
			}
			return n.Paused, nil
		})
	}
	return false, nil
}

func (k stillPaused) row(_ context.Context, key string, read func() (*model.Pause, error)) (bool, error) {
	if paused, ok := k.rows[key]; ok {
		return paused, nil
	}
	pause, err := read()
	if errors.Is(err, repository.ErrNotFound) {
		pause, err = nil, nil // gone: nothing holds its resources
	}
	if err != nil {
		return false, fmt.Errorf("cannot read %s: %w", key, err)
	}
	k.rows[key] = pause != nil
	return pause != nil, nil
}
