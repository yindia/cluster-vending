package subtenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

var _ cloudv1connect.SubTenantServiceHandler = (*Service)(nil)

func validateSpec(spec *v1.SubTenantSpec, groups config.NaaS) error {
	if spec == nil {
		return invalid("subtenant.spec is required")
	}
	if err := service.Validate(spec); err != nil {
		return err
	}
	if err := validateKeys(spec.NamespaceKeys); err != nil {
		return err
	}
	if err := validateQuota(int(spec.GetQuota().GetNamespaceCount()), spec.GetQuota().GetHard(), spec.NamespaceKeys); err != nil {
		return err
	}
	if len(spec.GetAccess().GetBindings()) != 0 {
		return connect.NewError(connect.CodeFailedPrecondition, unresolvedIdentities().Error())
	}
	if err := validateGroups(spec.GetAccess().GetAdminGroups(), spec.GetAccess().GetViewerGroups()); err != nil {
		return err
	}
	if err := allowedGroups(groups, spec.GetAccess().GetAdminGroups(), spec.GetAccess().GetViewerGroups()); err != nil {
		return err
	}
	return nil
}

func createIdentity(req *v1.CreateSubTenantRequest) (string, string, error) {
	requestID, err := uuid.Parse(req.RequestId)
	if err != nil {
		return "", "", invalid("request_id must be a stable UUID for safe retries")
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("subtenant\x00"+req.TenantId+"\x00"+requestID.String())).String()
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(&v1.CreateSubTenantRequest{TenantId: req.TenantId, ClusterId: req.ClusterId, Subtenant: &v1.SubTenant{Spec: req.GetSubtenant().GetSpec()}, RequestId: requestID.String()})
	if err != nil {
		return "", "", connect.NewError(connect.CodeInternal, "cannot encode allocation request")
	}
	hash := sha256.Sum256(payload)
	return id, hex.EncodeToString(hash[:]), nil
}

func (s *Service) get(ctx context.Context, registration *cloud.Tenant, clusterID, id string) (*cloud.SubTenant, error) {
	obj := &cloud.SubTenant{}
	if err := s.reader().Get(ctx, client.ObjectKey{Namespace: kube.TenantNamespace(registration.Name), Name: id}, obj); err != nil {
		return nil, kubeError(err)
	}
	if !belongs(obj, registration, clusterID) {
		return nil, connect.NewError(connect.CodeNotFound, "subtenant not found in this tenant and cluster")
	}
	return obj, nil
}
func belongs(obj *cloud.SubTenant, tenant *cloud.Tenant, clusterID string) bool {
	return obj.Namespace == kube.TenantNamespace(tenant.Name) && obj.Labels[cloudstore.LabelTenant] == tenant.Name && obj.Labels[cloudstore.LabelID] == obj.Name && obj.Spec.TenantRef.Name == tenant.Name && obj.Spec.TenantRef.UID == string(tenant.UID) && obj.Spec.ClusterRef.ID != "" && obj.Labels[cloudstore.LabelClusterID] == obj.Spec.ClusterRef.ID && (clusterID == "" || obj.Spec.ClusterRef.ID == clusterID)
}

func (s *Service) CreateSubTenant(ctx context.Context, req *v1.CreateSubTenantRequest) (*v1.CreateSubTenantResponse, error) {
	if err := s.access(req.GetTenantId(), req); err != nil {
		return nil, err
	}
	in := req.GetSubtenant()
	if in == nil || in.Spec == nil {
		return nil, invalid("subtenant.spec is required")
	}
	if in.Id != "" || in.TenantId != "" || in.ClusterId != "" || in.Status != nil || in.Etag != "" || in.Generation != 0 || in.CreateTime != nil || in.UpdateTime != nil {
		return nil, invalid("create accepts only subtenant.spec; identity and status are assigned by the server")
	}
	if err := validateSpec(in.Spec, s.deps.NaaS); err != nil {
		return nil, err
	}
	if in.Spec.Name == "" {
		return nil, invalid("spec.name is required for new allocations")
	}
	id, hash, err := createIdentity(req)
	if err != nil {
		return nil, err
	}
	registration, err := s.tenant(ctx, req.TenantId, true)
	if err != nil {
		return nil, err
	}
	parent, err := s.parent(ctx, req.TenantId, req.ClusterId)
	if err != nil {
		return nil, err
	}
	key := client.ObjectKey{Namespace: kube.TenantNamespace(req.TenantId), Name: id}
	existing := &cloud.SubTenant{}
	if err := s.reader().Get(ctx, key, existing); err == nil {
		return s.replayCreate(ctx, existing, registration, parent, hash)
	} else if !apierrors.IsNotFound(err) {
		return nil, kubeError(err)
	}
	if err := s.claimParent(ctx, parent); err != nil {
		return nil, err
	}
	if err := s.reserveName(ctx, key.Namespace, registration, parent, in.Spec.Name, id, hash); err != nil {
		return nil, err
	}
	desired := specToCR(in.Spec)
	desired.TenantRef = cloud.SubTenantTenantReference{Name: registration.Name, UID: string(registration.UID)}
	desired.ClusterRef = cloud.SubTenantClusterReference{ID: req.ClusterId, Name: parent.Name, UID: string(parent.UID)}
	labels := crossplane.Labels(req.TenantId)
	labels[cloudstore.LabelTenant] = req.TenantId
	labels[cloudstore.LabelID] = id
	labels[cloudstore.LabelClusterID] = req.ClusterId
	obj := &cloud.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: key.Namespace, Generation: 1, Labels: labels, Annotations: map[string]string{requestAnnotation: req.RequestId, hashAnnotation: hash, updatedAnnotation: time.Now().UTC().Format(time.RFC3339Nano)}}, Spec: desired}
	// This is only desired intent. A workload controller must atomically add
	// SubTenantFinalizer BEFORE any remote write; CRUD never assumes ownership
	// of that finalizer or creates an owner-GC cascade.
	if err := s.deps.Kube.Create(ctx, obj); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, kubeError(err)
		}
		if err := s.reader().Get(ctx, key, existing); err != nil {
			return nil, kubeError(err)
		}
		return s.replayCreate(ctx, existing, registration, parent, hash)
	}
	if err := s.bindNameClaim(ctx, obj, hash); err != nil {
		return nil, err
	}
	currentParent, parentErr := s.parent(ctx, req.TenantId, req.ClusterId)
	currentTenant, tenantErr := s.tenant(ctx, req.TenantId, true)
	// Fence parent writes prepared before the child became visible. Conflicts
	// retry only after revalidating the same parent incarnation and NaaS state.
	if parentErr == nil && parentUnchanged(parent, currentParent) {
		parentErr = s.claimParent(ctx, currentParent)
	}
	if parentErr != nil || tenantErr != nil || !parentUnchanged(parent, currentParent) || currentTenant.UID != registration.UID {
		// Never strip finalizers: a controller could have observed the CR already.
		cleanupErr := s.deps.Kube.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground), client.Preconditions{ResourceVersion: &obj.ResourceVersion})
		if cleanupErr != nil && !apierrors.IsNotFound(cleanupErr) {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Sprintf("parent changed and allocation %s could not be marked for cleanup; inspect it before retrying", id))
		}
		remaining := &cloud.SubTenant{}
		if err := s.reader().Get(ctx, key, remaining); apierrors.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeAborted, "parent changed during creation; unprovisioned allocation intent was removed")
		}
		return nil, connect.NewError(connect.CodeAborted, fmt.Sprintf("parent changed during creation; allocation %s has cancellation requested and remains protected until controller cleanup", id))
	}
	return &v1.CreateSubTenantResponse{Subtenant: resourceView(obj)}, nil
}

func (s *Service) replayCreate(ctx context.Context, obj *cloud.SubTenant, tenant *cloud.Tenant, parent *cloud.Cluster, hash string) (*v1.CreateSubTenantResponse, error) {
	if !belongs(obj, tenant, parent.Labels[cloudstore.LabelID]) || obj.Spec.ClusterRef.UID != string(parent.UID) || obj.Spec.ClusterRef.Name != parent.Name {
		return nil, connect.NewError(connect.CodeAlreadyExists, "request_id belongs to another parent incarnation or allocation")
	}
	if obj.Annotations[hashAnnotation] != hash {
		return nil, connect.NewError(connect.CodeAlreadyExists, "request_id was already used with a different allocation payload")
	}
	if !obj.DeletionTimestamp.IsZero() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "the allocation for this request_id is being deleted")
	}
	if err := s.reserveName(ctx, obj.Namespace, tenant, parent, obj.Spec.Name, obj.Name, hash); err != nil {
		return nil, err
	}
	if err := s.bindNameClaim(ctx, obj, hash); err != nil {
		return nil, err
	}
	return &v1.CreateSubTenantResponse{Subtenant: resourceView(obj)}, nil
}

func (s *Service) GetSubTenant(ctx context.Context, req *v1.GetSubTenantRequest) (*v1.GetSubTenantResponse, error) {
	if err := s.access(req.GetTenantId(), req); err != nil {
		return nil, err
	}
	tenant, err := s.tenant(ctx, req.TenantId, false)
	if err != nil {
		return nil, err
	}
	obj, err := s.get(ctx, tenant, req.ClusterId, req.SubtenantId)
	if err != nil {
		return nil, err
	}
	return &v1.GetSubTenantResponse{Subtenant: resourceView(obj)}, nil
}

func (s *Service) UpdateSubTenant(ctx context.Context, req *v1.UpdateSubTenantRequest) (*v1.UpdateSubTenantResponse, error) {
	if err := s.access(req.GetTenantId(), req); err != nil {
		return nil, err
	}
	in := req.GetSubtenant()
	if in == nil || in.Id == "" || in.Etag == "" {
		return nil, invalid("subtenant.id and etag are required")
	}
	if in.TenantId != "" && in.TenantId != req.TenantId || in.ClusterId != "" && in.ClusterId != req.ClusterId {
		return nil, invalid("tenant and parent cluster are immutable")
	}
	unprotectOnly := len(req.GetUpdateMask().GetPaths()) == 1 && req.UpdateMask.Paths[0] == "spec.deletion_protection" && in.GetSpec() != nil && in.Spec.DeletionProtection != nil && !in.Spec.GetDeletionProtection()
	tenant, err := s.tenant(ctx, req.TenantId, !unprotectOnly)
	if err != nil {
		return nil, err
	}
	var parent *cloud.Cluster
	if !unprotectOnly {
		parent, err = s.parent(ctx, req.TenantId, req.ClusterId)
		if err != nil {
			return nil, err
		}
	}
	obj, err := s.get(ctx, tenant, req.ClusterId, in.Id)
	if err != nil {
		return nil, err
	}
	if parent != nil && (obj.Spec.ClusterRef.UID != string(parent.UID) || obj.Spec.ClusterRef.Name != parent.Name) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "the original parent cluster no longer exists")
	}
	if !obj.DeletionTimestamp.IsZero() && !unprotectOnly {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "subtenant is being deleted")
	}
	if obj.ResourceVersion != in.Etag {
		return nil, connect.NewError(connect.CodeAborted, "etag mismatch; refresh the allocation before updating")
	}
	merged := specFromCR(obj.Spec)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, invalid("update_mask must contain explicit supported spec paths")
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			return nil, invalid("duplicate update mask path %q", path)
		}
		seen[path] = true
		switch path {
		case "spec.display_name":
			merged.DisplayName = in.GetSpec().GetDisplayName()
		case "spec.namespace_keys":
			merged.NamespaceKeys = append([]string(nil), in.GetSpec().GetNamespaceKeys()...)
		case "spec.quota":
			merged.Quota = proto.CloneOf(in.GetSpec().GetQuota())
		case "spec.policy_profile":
			merged.PolicyProfile = in.GetSpec().GetPolicyProfile()
		case "spec.access":
			merged.Access = proto.CloneOf(in.GetSpec().GetAccess())
		case "spec.deletion_protection":
			if in.GetSpec() == nil || in.Spec.DeletionProtection == nil {
				return nil, invalid("spec.deletion_protection must be explicitly true or false")
			}
			merged.DeletionProtection = ptr.To(in.Spec.GetDeletionProtection())
		default:
			return nil, invalid("unsupported update mask %q", path)
		}
	}
	if !unprotectOnly {
		if err := validateSpec(merged, s.deps.NaaS); err != nil {
			return nil, err
		}
	}
	if err := appendOnlyKeys(specFromCR(obj.Spec).NamespaceKeys, merged.NamespaceKeys); err != nil {
		return nil, err
	}
	if proto.Equal(merged, specFromCR(obj.Spec)) {
		return &v1.UpdateSubTenantResponse{Subtenant: resourceView(obj)}, nil
	}
	before := obj.DeepCopy()
	desired := specToCR(merged)
	desired.TenantRef = obj.Spec.TenantRef
	desired.ClusterRef = obj.Spec.ClusterRef
	obj.Spec = desired
	obj.Generation++
	obj.Annotations = maps.Clone(obj.Annotations)
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations[updatedAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	if parent != nil {
		if err := s.claimParent(ctx, parent); err != nil {
			return nil, err
		}
	}
	if err := s.deps.Kube.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, kubeError(err)
	}
	return &v1.UpdateSubTenantResponse{Subtenant: resourceView(obj)}, nil
}

func (s *Service) DeleteSubTenant(ctx context.Context, req *v1.DeleteSubTenantRequest) (*v1.DeleteSubTenantResponse, error) {
	if err := s.access(req.GetTenantId(), req); err != nil {
		return nil, err
	}
	tenant, err := s.tenant(ctx, req.TenantId, false)
	if err != nil {
		return nil, err
	}
	obj, err := s.get(ctx, tenant, req.ClusterId, req.SubtenantId)
	if err != nil {
		return nil, err
	}
	if req.Etag == "" || obj.ResourceVersion != req.Etag {
		return nil, connect.NewError(connect.CodeAborted, "etag mismatch; refresh before deleting")
	}
	if obj.Spec.DeletionProtection == nil || *obj.Spec.DeletionProtection {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "deletion protection is enabled; explicitly update spec.deletion_protection to false before deleting")
	}
	if !obj.DeletionTimestamp.IsZero() {
		return &v1.DeleteSubTenantResponse{Subtenant: resourceView(obj)}, nil
	}
	if len(obj.Status.DeletionBlockers) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "deletion is blocked: "+fmt.Sprint(obj.Status.DeletionBlockers))
	}
	if !slices.Contains(obj.Finalizers, cloud.SubTenantFinalizer) && remoteStateRecorded(obj) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "remote allocation state is recorded without the controller protection finalizer; deletion is refused until protection and safe cleanup are restored")
	}
	snapshot := obj.DeepCopy()
	snapshot.DeletionTimestamp = ptr.To(metav1.Now())
	if err := s.deps.Kube.Delete(ctx, obj, client.Preconditions{ResourceVersion: &obj.ResourceVersion}, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		return nil, kubeError(err)
	}
	if err := s.reader().Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) {
			view := resourceView(snapshot)
			view.Status.Message = "Deletion accepted; the unprovisioned allocation intent was removed. No workload-cluster namespaces were deleted."
			for _, condition := range view.Status.Conditions {
				if condition.Type == "Ready" {
					condition.Reason = "IntentDeleted"
					condition.Message = view.Status.Message
				}
			}
			return &v1.DeleteSubTenantResponse{Subtenant: view}, nil
		}
		return nil, kubeError(err)
	}
	return &v1.DeleteSubTenantResponse{Subtenant: resourceView(obj)}, nil
}

// Without a protection finalizer only demonstrably unprovisioned intent may
// disappear. Missing observations never authorize deletion of recorded data.
func remoteStateRecorded(obj *cloud.SubTenant) bool {
	st := obj.Status
	if st.RemoteCapsuleTenant != "" || st.RemoteCapsuleTenantUID != "" || st.TemplateRevision != "" {
		return true
	}
	if st.State != "" && st.State != cloud.SubTenantStatePending {
		return true
	}
	for _, namespace := range st.Namespaces {
		if namespace.Name != "" || namespace.UID != "" || namespace.TemplateRevision != "" {
			return true
		}
	}
	for _, condition := range st.Conditions {
		if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}
