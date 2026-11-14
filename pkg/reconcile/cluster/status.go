package cluster

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Condition types of ClusterStatus.
const (
	conditionReady  = "Ready"
	conditionSynced = "Synced"
)

// Encode is m as the JSON a row stores (protojson).
func Encode(m proto.Message) (model.JSON, error) {
	raw, err := protojson.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode %T: %w", m, err)
	}
	return model.JSON(raw), nil
}

// Lenient reads stored JSON: fields the contract dropped since (e.g.
// spec.tags, now labels) are skipped, not errors.
var Lenient = protojson.UnmarshalOptions{DiscardUnknown: true}

// SpecOf decodes the stored (resolved) spec.
func SpecOf(row *model.Cluster) (*cloudv1.ClusterSpec, error) {
	spec := &cloudv1.ClusterSpec{}
	if err := Lenient.Unmarshal(row.Spec, spec); err != nil {
		return nil, fmt.Errorf("cluster %s: stored spec: %w", row.ID, err)
	}
	return spec, nil
}

// View is the API view of a stored cluster: spec, the live state of its
// managed resources, and what EKS reports (endpoint, version, ARNs).
//
// A failed attempt to apply the cluster is part of it (the failure the
// last pass recorded with the row): nothing else says so while there are
// no resources to observe.
func (s *Reconciler) View(ctx context.Context, row *model.Cluster) (*cloudv1.Cluster, error) {
	return s.view(ctx, row, storedFailure(row))
}

// view is View with the failure to report: the recorded one for a read,
// this pass's for the reconciler.
func (s *Reconciler) view(ctx context.Context, row *model.Cluster, failure *cloudv1.Condition) (*cloudv1.Cluster, error) {
	return s.viewWith(ctx, row, failure, nil)
}

// viewWith is view with the platform components this pass found; nil
// reports those recorded with the row (a read, or a pass that did not
// look).
func (s *Reconciler) viewWith(ctx context.Context, row *model.Cluster, failure *cloudv1.Condition, installed *platform) (*cloudv1.Cluster, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return nil, storeError(err)
	}
	c, err := s.viewOf(ctx, row, records)
	if err != nil {
		return nil, err
	}
	withFailure(c.GetStatus(), failure)
	if installed == nil {
		installed = storedPlatform(row)
	}
	withPlatform(c.GetStatus(), installed)
	s.withKubernetes(ctx, row.ID, c.GetSpec(), c.GetStatus(), storedStatus(row))
	s.withUpgradePause(ctx, row, c.GetSpec(), c.GetStatus())
	// What the controller reported last stays with the status: a read
	// returns it, and a pass adds to it (remember).
	c.GetStatus().RecentEvents = storedStatus(row).GetRecentEvents()
	return c, nil
}

// viewOf is view with the cluster's records at hand. It only reads.
func (s *Reconciler) viewOf(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (*cloudv1.Cluster, error) {
	spec, err := SpecOf(row)
	if err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return nil, err
	}
	if row.State == StateProvisioning || row.State == StateUpdating {
		reconcile.SettleMissing(records, views)
	}
	st := status(row, views, !createdAfterActive(records))
	if err := s.observeEKS(ctx, records, st); err != nil {
		s.logger().ErrorContext(ctx, "observe eks", "cluster_id", row.ID, "error", err)
		return nil, fault.New(fault.Unavailable, "cannot read the cluster from the management cluster")
	}
	u, err := s.observeUpgrade(ctx, records, spec.GetControlPlane().GetKubernetesVersion())
	if err != nil {
		s.logger().ErrorContext(ctx, "observe upgrade", "cluster_id", row.ID, "error", err)
		return nil, fault.New(fault.Unavailable, "cannot read the cluster from the management cluster")
	}
	st.Upgrade = u.status(row, views, storedUpgrade(row))
	return ClusterProto(row, spec, st), nil
}

// observeEKS copies what the EKS Cluster and NodeGroup report into st.
func (s *Reconciler) observeEKS(ctx context.Context, records []model.ManagedResource, st *cloudv1.ClusterStatus) error {
	aws := &cloudv1.AwsClusterStatus{}
	views := make(map[string]*cloudv1.ManagedResource, len(st.GetManagedResources()))
	for _, v := range st.GetManagedResources() {
		views[v.GetKind()+"/"+v.GetName()] = v
	}
	for _, r := range records {
		key := client.ObjectKey{Namespace: r.Namespace, Name: r.Name}
		switch r.Kind {
		case "Cluster":
			c := &awseks.Cluster{}
			if err := s.getIgnoreMissing(ctx, key, c); err != nil {
				return err
			}
			fromEKSCluster(c.Status.AtProvider, st, aws)
		case "ClusterAuth":
			if views[r.Kind+"/"+r.Name].GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
				st.KubeconfigSecret = r.Namespace + "/" + kubeconfigSecret(r.Name)
			}
		case "NodeGroup":
			ng := &awseks.NodeGroup{}
			if err := s.getIgnoreMissing(ctx, key, ng); err != nil {
				return err
			}
			aws.NodeGroupArn = deref(ng.Status.AtProvider.Arn)
		case "Addon":
			addon := &awseks.Addon{}
			if err := s.getIgnoreMissing(ctx, key, addon); err != nil {
				return err
			}
			v := views[r.Kind+"/"+r.Name]
			aws.Addons = append(aws.Addons, &cloudv1.AwsAddonStatus{
				Name: addonName(r.Name), Version: deref(addon.Status.AtProvider.AddonVersion),
				State: v.GetState(), Message: v.GetMessage(),
			})
		}
	}
	st.Provider = &cloudv1.ClusterStatus_Aws{Aws: aws}
	return nil
}

func (s *Reconciler) getIgnoreMissing(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	err := s.deps.Get(ctx, key, obj)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	return err
}

func fromEKSCluster(at awseks.ClusterObservation, st *cloudv1.ClusterStatus, aws *cloudv1.AwsClusterStatus) {
	aws.ClusterArn = deref(at.Arn)
	aws.PlatformVersion = deref(at.PlatformVersion)
	if at.VPCConfig != nil {
		aws.ClusterSecurityGroupId = deref(at.VPCConfig.ClusterSecurityGroupID)
	}
	st.CurrentKubernetesVersion = deref(at.Version)
	for _, id := range at.Identity {
		for _, o := range id.Oidc {
			st.OidcIssuerUrl = deref(o.Issuer)
		}
	}
	if url := deref(at.Endpoint); url != "" {
		st.Endpoint = &cloudv1.ClusterEndpoint{Url: url}
		for _, ca := range at.CertificateAuthority {
			// EKS returns the PEM base64-encoded; the API carries the PEM.
			st.Endpoint.CertificateAuthorityData = decodeCA(deref(ca.Data))
		}
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// status aggregates the managed resources' states: RUNNING once all are
// READY, FAILED when one failed or vanished, else the stored lifecycle
// state (PROVISIONING, UPDATING, DELETING). awaitingEKS: the add-ons and
// access entries are not created yet (they follow once EKS is ACTIVE), so
// the cluster cannot be RUNNING.
func status(row *model.Cluster, views []*cloudv1.ManagedResource, awaitingEKS bool) *cloudv1.ClusterStatus {
	converged := convergedOn(row, views)
	st := &cloudv1.ClusterStatus{ManagedResources: reconcile.ExplainMissing(views, row.State == StateDeleting),
		ObservedGeneration: row.ObservedGeneration}
	if converged {
		st.ObservedGeneration = row.Generation
	}
	ready, waiting, left := 0, 0, 0
	var failed *cloudv1.ManagedResource
	for _, v := range views {
		switch v.GetState() {
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY:
			ready++
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING:
			waiting++
		case cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING:
			if failed == nil && (row.State != StateUpdating || !versionSkew(v)) {
				failed = v
			}
		default:
		}
		if v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			left++
		}
	}
	progress := fmt.Sprintf("%d of %d resources ready", ready, len(views))
	if waiting > 0 {
		progress += fmt.Sprintf(", %d waiting on others", waiting)
	}
	readyCond := &cloudv1.Condition{Type: conditionReady, Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: "ResourcesNotReady", Message: progress}
	st.Import = StoredImport(row)
	switch {
	case row.State == StateDeleting:
		st.State = cloudv1.ClusterState_CLUSTER_STATE_DELETING
		st.Message = deletingMessage(views, left)
	case row.State == StateBlocked:
		st.State = cloudv1.ClusterState_CLUSTER_STATE_FAILED
		st.Message = "import blocked: " + firstBlocking(st.GetImport())
	case failed != nil:
		st.State = cloudv1.ClusterState_CLUSTER_STATE_FAILED
		st.Message = fmt.Sprintf("%s %s: %s", failed.GetKind(), failed.GetName(), failed.GetMessage())
		st.Conditions = append(st.Conditions, &cloudv1.Condition{Type: conditionSynced,
			Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "ReconcileError", Message: st.Message})
	case discovering(row):
		st.State, st.Message = cloudv1.ClusterState_CLUSTER_STATE_IMPORTED, discoveringMessage
	case row.State == StateImported:
		st.State = cloudv1.ClusterState_CLUSTER_STATE_IMPORTED
		st.Message = "imported read-only (" + progress + "); review status.import, then TakeOverCluster"
	case awaitingEKS:
		st.State = cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING
		st.Message = "creating the EKS cluster (" + progress + "); add-ons and access entries follow once it is ACTIVE"
	case len(views) > 0 && ready == len(views) && converged:
		st.State = cloudv1.ClusterState_CLUSTER_STATE_RUNNING
		readyCond.Status, readyCond.Reason = cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, "AllResourcesReady"
		if imp := st.GetImport(); imp.GetPhase() == cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING {
			imp.Phase = cloudv1.ImportPhase_IMPORT_PHASE_CONVERGED
		}
	case len(views) > 0 && ready == len(views):
		st.State, st.Message = cloudv1.ClusterState_CLUSTER_STATE_UPDATING,
			fmt.Sprintf("applying generation %d (%s)", row.Generation, progress)
	case row.State == StateUpdating, row.State == StateRunning: // running, not ready: reconciling a change
		st.State, st.Message = cloudv1.ClusterState_CLUSTER_STATE_UPDATING, progress
	default:
		st.State, st.Message = cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING, progress
	}
	if paused, note := reconcile.RowPause(row.Paused, views); paused != nil {
		// A pause is the operator's: reported, never a failure. A paused
		// cluster says who paused it whatever its state; paused resources
		// of a cluster that is not are only worth a note while it moves.
		st.Conditions = append(st.Conditions, paused)
		st.Pause = reconcile.PauseProto(row.Paused)
		if row.Paused != nil || st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_RUNNING {
			st.Message = strings.TrimPrefix(st.Message+note, "; ")
		}
	}
	withEditIgnored(st, row)
	st.Conditions = append([]*cloudv1.Condition{readyCond}, st.Conditions...)
	return st
}

func firstBlocking(imp *cloudv1.ImportStatus) string {
	for _, l := range imp.GetResources() {
		if l.GetDisposition() == cloudv1.Disposition_DISPOSITION_BLOCKING {
			return l.GetDetail()
		}
	}
	return "see status.import"
}

func decodeCA(b64 string) []byte {
	pem, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return []byte(b64)
	}
	return pem
}

// kubeconfigSecret is the Secret of a ClusterAuth object "<prefix>-auth".
func kubeconfigSecret(auth string) string {
	return crossplaneaws.KubeconfigSecretName(strings.TrimSuffix(auth, "-auth"))
}

// addonName is the add-on of an Addon object "<prefix>-addon-<name>".
func addonName(object string) string {
	if i := strings.LastIndex(object, addonInfix); i >= 0 {
		return object[i+len(addonInfix):]
	}
	return object
}

const addonInfix = "-addon-"

// deletionUsers are the kinds other objects' deletion waits for (the "by"
// side of the cluster's Usages), outermost first: while they exist, what
// they use stays.
var deletionUsers = []string{"NodeGroup", "Cluster", "Addon"}

// deletingMessage says where a deletion stands: the users still holding
// the rest (the stage), and the objects whose deletion is stuck (Synced
// False), with the error and for how long.
func deletingMessage(views []*cloudv1.ManagedResource, left int) string {
	msg := fmt.Sprintf("deleting; %d resources left", left)
	for _, kind := range deletionUsers {
		var names []string
		for _, v := range views {
			if v.GetKind() == kind && v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
				names = append(names, v.GetName())
			}
		}
		if len(names) > 0 {
			msg = fmt.Sprintf("deleting; waiting for %s %s; %d resources left", kind, strings.Join(names, ", "), left)
			break
		}
	}
	var stuck []string
	for _, v := range views {
		if v.GetSynced() != cloudv1.ConditionStatus_CONDITION_STATUS_FALSE ||
			v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING || v.GetDeletionTime() == nil {
			continue
		}
		since := now().Sub(v.GetDeletionTime().AsTime()).Round(time.Minute)
		stuck = append(stuck, fmt.Sprintf("%s %s (%s: %s) for %s", v.GetKind(), v.GetName(), v.GetReason(), v.GetMessage(), since))
	}
	if len(stuck) > 0 {
		msg += "; stuck: " + strings.Join(stuck, "; ")
	}
	return msg
}

// now is the clock; tests replace it.
var now = time.Now

// convergedOn reports whether the cluster runs its latest generation: all
// of it was applied (AppliedGeneration) and every object's provider has
// reconciled the object's latest spec (its Synced condition's
// observedGeneration is its metadata.generation).
func convergedOn(row *model.Cluster, views []*cloudv1.ManagedResource) bool {
	if row.AppliedGeneration < row.Generation {
		return false
	}
	for _, v := range views {
		if v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING ||
			v.GetSynced() == cloudv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED { // no provider status (yet)
			continue
		}
		if reconcile.Behind(v) {
			return false
		}
	}
	return true
}

// versionSkew reports whether v fails because its Kubernetes version is
// ahead of the control plane's: EKS refuses node groups and add-ons newer
// than the cluster while the control plane upgrades. Expected during an
// upgrade, not a failure.
func versionSkew(v *cloudv1.ManagedResource) bool {
	if v.GetKind() != "NodeGroup" && v.GetKind() != "Addon" {
		return false
	}
	msg := strings.ToLower(v.GetMessage())
	return strings.Contains(msg, "version") && (strings.Contains(msg, "cluster") || strings.Contains(msg, "kubernetes"))
}
