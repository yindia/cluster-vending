// Package subtenant implements tenant-scoped SubTenant RPCs using management
// cluster custom resources. CRUD records desired state; it never creates or
// deletes workload-cluster namespaces and never claims reconciliation is ready.
package subtenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	tenantlife "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

type Service struct{ deps reconcile.Deps }

// NewService uses Kubernetes only; no repository or database is required.
func NewService(d reconcile.Deps) *Service { return &Service{deps: d} }

func (s *Service) reader() client.Reader {
	if s.deps.KubeReader != nil {
		return s.deps.KubeReader
	}
	return s.deps.Kube
}

// access checks a request: a valid tenant ID, valid fields, none unknown.
// It checks no caller: the API has no roles.
func (s *Service) access(tenant string, req proto.Message) error {
	if !tenantlife.ValidID(tenant) {
		return connect.NewError(connect.CodeInvalidArgument, "tenant_id must be a valid tenant DNS label")
	}
	if err := service.Validate(req); err != nil {
		return err
	}
	if unknown(req.ProtoReflect()) {
		return connect.NewError(connect.CodeInvalidArgument, "unknown request fields are not supported")
	}
	if s.deps.Kube == nil || s.reader() == nil {
		return connect.NewError(connect.CodeUnavailable, "management cluster is unavailable")
	}
	return nil
}

func unknown(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	found := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsMap() && field.MapValue().Kind() == protoreflect.MessageKind:
			value.Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool { found = unknown(v.Message()); return !found })
		case field.IsList() && field.Kind() == protoreflect.MessageKind:
			for i := 0; i < value.List().Len(); i++ {
				if unknown(value.List().Get(i).Message()) {
					found = true
					break
				}
			}
		case field.Kind() == protoreflect.MessageKind && !field.IsMap() && !field.IsList():
			found = unknown(value.Message())
		}
		return !found
	})
	return found
}

func kubeError(err error) error {
	var connected *connect.Error
	if errors.As(err, &connected) {
		return err
	}
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return connect.NewError(connect.CodeNotFound, "resource not found in the requested tenant and cluster")
	case apierrors.IsAlreadyExists(err):
		return connect.NewError(connect.CodeAlreadyExists, "subtenant already exists")
	case apierrors.IsConflict(err):
		return connect.NewError(connect.CodeAborted, "resource changed; refresh and retry with its current etag")
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return service.ToConnect(err)
	default:
		return connect.NewError(connect.CodeUnavailable, "management cluster request failed")
	}
}

func (s *Service) tenant(ctx context.Context, id string, write bool) (*cloud.Tenant, error) {
	registration := &cloud.Tenant{}
	if err := s.reader().Get(ctx, client.ObjectKey{Name: id}, registration); err != nil {
		return nil, kubeError(err)
	}
	if registration.UID == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "tenant registration has no stable UID")
	}
	ns := &corev1.Namespace{}
	if err := s.reader().Get(ctx, client.ObjectKey{Name: kube.TenantNamespace(id)}, ns); err != nil {
		return nil, kubeError(err)
	}
	if ns.Labels[cloudstore.LabelTenant] != id || ns.Annotations[tenantlife.AnnotationRegistration] != string(registration.UID) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "tenant namespace is not owned by the current tenant registration")
	}
	if write && (!registration.DeletionTimestamp.IsZero() || !ns.DeletionTimestamp.IsZero() || ns.Status.Phase == corev1.NamespaceTerminating) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "tenant registration or namespace is being deleted")
	}
	return registration, nil
}

func (s *Service) parent(ctx context.Context, tenant, clusterID string) (*cloud.Cluster, error) {
	items := &cloud.ClusterList{}
	if err := s.reader().List(ctx, items, client.InNamespace(kube.TenantNamespace(tenant)), client.MatchingLabels{cloudstore.LabelTenant: tenant, cloudstore.LabelID: clusterID}); err != nil {
		return nil, kubeError(err)
	}
	if len(items.Items) != 1 {
		return nil, connect.NewError(connect.CodeNotFound, "parent cluster not found in this tenant")
	}
	parent := &items.Items[0]
	if parent.UID == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "parent cluster has no stable UID")
	}
	var metadata struct {
		State string `json:"state"`
	}
	if raw := parent.Annotations[cloudstore.AnnotationRow]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, "parent cluster metadata cannot be safely read")
		}
	}
	if !parent.DeletionTimestamp.IsZero() || parent.Status.Phase == cloud.ClusterPhaseDeleting || metadata.State == "CLUSTER_STATE_DELETING" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "parent cluster is being deleted")
	}
	if parent.Spec.NaaS == nil || !*parent.Spec.NaaS {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "parent cluster must have spec.naas enabled")
	}
	return parent, nil
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf(format, args...))
}
