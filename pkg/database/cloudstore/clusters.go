package cloudstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/owner"
)

type clusters struct{ store }

// AnnotationRowGeneration is the object generation (metadata.generation) the
// store's last write of the object produced.
const AnnotationRowGeneration = group + "row-generation"

// Stored states of a cluster the store itself looks at.
const (
	clusterImported = "CLUSTER_STATE_IMPORTED"
	// clusterBlocked is the stored state of an import that was blocked.
	clusterBlocked = "CLUSTER_STATE_FAILED"
)

// clusterKeys are the claims of a cluster: its create request ID. Its
// name is the object's name, unique in the tenant by itself.
func clusterKeys(c *model.Cluster) []string {
	if c.CreateRequestID == nil {
		return nil
	}
	return []string{"cluster-request/" + *c.CreateRequestID}
}

// clusterImport is spec.importFrom of a cluster row: nil for a cluster
// the platform created (networkImport, for a cluster).
func clusterImport(c *model.Cluster, stored *cloudv1alpha1.ClusterImport) *cloudv1alpha1.ClusterImport {
	out := stored.DeepCopy()
	req := &cloudv1.ImportClusterRequest{}
	if err := decode(c.Input, req); err == nil && req.GetAws().GetDefaultNodeGroupName() != "" {
		out = &cloudv1alpha1.ClusterImport{
			AWS: &cloudv1alpha1.AWSImportCluster{DefaultNodeGroupName: req.GetAws().GetDefaultNodeGroupName()}}
	}
	if out == nil {
		return nil
	}
	takenOver := c.State != clusterImported && c.State != clusterBlocked || stored != nil && stored.TakeOver
	out.Takeover = cloudv1alpha1.Takeover{TakeOver: takenOver, AcknowledgeNotImported: takenOver}
	return out
}

// fill writes the row onto obj: spec, labels and annotations, everything
// but the status. networkName is the Network object of the row's network.
func (r clusters) fill(c *model.Cluster, networkName string, obj *cloudv1alpha1.Cluster) error {
	p := &cloudv1.ClusterSpec{}
	if err := decode(c.Spec, p); err != nil {
		return fmt.Errorf("%w: cluster spec: %w", repository.ErrInvalid, err)
	}
	spec, remainder := convert.ClusterSpecToAPI(p, networkName)
	spec.DisplayName, spec.Tags, spec.ImportFrom = c.DisplayName, cloudv1alpha1.Tags(c.Labels), clusterImport(c, obj.Spec.ImportFrom)
	if spec.Region == "" {
		spec.Region = c.Region
	}
	obj.Spec = spec
	label(obj, map[string]string{LabelID: c.ID, LabelTenant: c.TenantID, LabelAccountID: c.CloudAccountID, LabelNetworkID: c.NetworkID})
	rest, err := remainderJSON(remainder != nil, remainder)
	if err != nil {
		return err
	}
	annotate(obj, map[string]string{AnnotationInput: string(c.Input), AnnotationRemainder: rest, AnnotationAdoption: string(c.Adoption)})
	m := resourceMeta(&c.Resource)
	m.Provider, m.CreateRequestID, m.Imported, m.DeleteForce = string(c.Provider), c.CreateRequestID, c.Imported, c.DeleteForce
	m.ResolvedGeneration = c.ResolvedGeneration
	return setMeta(obj, m)
}

// row is the row an object holds.
func (r clusters) row(obj *cloudv1alpha1.Cluster) (*model.Cluster, error) {
	remainder := &cloudv1.ClusterSpec{}
	if err := decode([]byte(obj.Annotations[AnnotationRemainder]), remainder); err != nil {
		return nil, fmt.Errorf("cluster %s: stored spec remainder: %w", obj.Name, err)
	}
	spec, err := protojson.Marshal(convert.ClusterSpecFromAPI(obj.Spec, obj.Labels[LabelNetworkID], remainder))
	if err != nil {
		return nil, err
	}
	m := metaOf(obj)
	c := &model.Cluster{
		Resource: model.Resource{Base: model.Base{ID: obj.Labels[LabelID]}, DisplayName: obj.Spec.DisplayName,
			Labels: model.Labels(obj.Spec.Tags), Spec: spec, Status: contractStatus(obj.Status.ContractStatus)},
		TenantID: obj.Labels[LabelTenant], Name: obj.Name, Input: model.JSON(obj.Annotations[AnnotationInput]),
		NetworkID: obj.Labels[LabelNetworkID], CloudAccountID: obj.Labels[LabelAccountID],
		Provider: model.Provider(m.Provider), Region: obj.Spec.Region, Imported: m.Imported,
		CreateRequestID: m.CreateRequestID, DeleteForce: m.DeleteForce, ResolvedGeneration: m.ResolvedGeneration,
	}
	if adoption := obj.Annotations[AnnotationAdoption]; adoption != "" {
		c.Adoption = model.JSON(adoption)
	}
	if d := convert.DeprecationsFromAPI(obj.Status.Deprecations); d != nil {
		if c.Deprecations, err = protojson.Marshal(d); err != nil {
			return nil, err
		}
	}
	c.Results = resultRefsFromAPI(obj.Status.Results)
	if ro := convert.RolloutFromAPI(obj.Status.Rollout); ro != nil {
		if c.Rollout, err = protojson.Marshal(ro); err != nil {
			return nil, err
		}
	}
	if rec := convert.RecommendationsFromAPI(obj.Status.Recommendations); rec != nil {
		if c.Recommendations, err = protojson.Marshal(rec); err != nil {
			return nil, err
		}
	}
	m.into(&c.Resource)
	c.Seen, c.SeenGeneration = seen(obj), obj.Generation
	c.Retry = retryOf(obj)
	return c, nil
}

// setStatus writes a row's status onto obj.Status: the contract status
// verbatim, and the typed fields read from it. The inventory and the
// deprecation scan (its result and its condition) stay.
//
// observed is the object generation the status acknowledges; nil leaves
// status.observedGeneration as it is. It is never taken from obj: obj may
// have been fetched after an edit that the writer never saw, and stamping
// that edit as observed would lose it (see write).
func (r clusters) setStatus(obj *cloudv1alpha1.Cluster, raw model.JSON, state string, observed *int64) {
	p := &cloudv1.ClusterStatus{}
	_ = decode(raw, p) // a status this build cannot read is still kept verbatim
	st := &obj.Status
	scan := meta.FindStatusCondition(st.Conditions, cloudv1alpha1.ConditionDeprecationScan)
	st.ContractStatus = keepStatus(st.ContractStatus, raw)
	convert.ClusterStatusInto(p, state, metav1.NewTime(now()), st)
	if scan != nil {
		meta.SetStatusCondition(&st.Conditions, *scan)
	}
	if observed != nil {
		st.ObservedGeneration = *observed
	}
}

// SetDeprecations writes the scan result and its DeprecationScan
// condition; no change when it says what is stored.
func (r clusters) SetDeprecations(ctx context.Context, tenantID, id string, deprecations model.JSON) error {
	p := &cloudv1.ClusterDeprecations{}
	if err := decode(deprecations, p); err != nil {
		return fmt.Errorf("%w: cluster deprecations: %w", repository.ErrInvalid, err)
	}
	return translate(retry(func() error {
		obj, err := r.find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		before := obj.Status.DeepCopy()
		obj.Status.Deprecations = convert.DeprecationsToAPI(p)
		meta.SetStatusCondition(&obj.Status.Conditions, convert.DeprecationCondition(obj.Status.Deprecations, obj.Generation))
		if equality.Semantic.DeepEqual(before, &obj.Status) {
			return nil
		}
		return r.c.Status().Update(ctx, obj)
	}))
}

// SetRollout writes the cluster's rollout record; no change writes
// nothing. The etag, the generation and the rest of the status stay.
func (r clusters) SetRollout(ctx context.Context, tenantID, id string, rollout model.JSON) error {
	p := &cloudv1.ClusterRollout{}
	if err := decode(rollout, p); err != nil {
		return fmt.Errorf("%w: cluster rollout: %w", repository.ErrInvalid, err)
	}
	return translate(retry(func() error {
		obj, err := r.find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		next := convert.RolloutToAPI(p)
		if equality.Semantic.DeepEqual(obj.Status.Rollout, next) {
			return nil
		}
		obj.Status.Rollout = next
		return r.c.Status().Update(ctx, obj)
	}))
}

// SetRecommendations writes the summary of the recorded usage; no change
// when it says what is stored.
func (r clusters) SetRecommendations(ctx context.Context, tenantID, id string, recommendations model.JSON) error {
	p := &cloudv1.ClusterRecommendations{}
	if err := decode(recommendations, p); err != nil {
		return fmt.Errorf("%w: cluster recommendations: %w", repository.ErrInvalid, err)
	}
	return translate(retry(func() error {
		obj, err := r.find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		next := convert.RecommendationsToAPI(p)
		if equality.Semantic.DeepEqual(obj.Status.Recommendations, next) {
			return nil
		}
		obj.Status.Recommendations = next
		return r.c.Status().Update(ctx, obj)
	}))
}

// usageHistoryKey is the key of the recorded usage in its ConfigMap.
const usageHistoryKey = "history.json.gz"

// usageHistoryName is the ConfigMap of a cluster's recorded usage, next
// to the Cluster object in the tenant namespace.
func usageHistoryName(cluster string) string { return cluster + "-usage-history" }

// UsageHistory reads the cluster's recorded usage; nil when there is none.
func (r clusters) UsageHistory(ctx context.Context, tenantID, id string) ([]byte, error) {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return nil, translate(err)
	}
	cm := &corev1.ConfigMap{}
	err = r.c.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: usageHistoryName(obj.Name)}, cm)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, translate(err)
	}
	if cm.Labels[LabelID] != id {
		return nil, nil // another cluster's of the same name, left behind
	}
	return cm.BinaryData[usageHistoryKey], nil
}

// deleteUsageHistory deletes the cluster's recorded usage ConfigMap (none is
// no error). Another cluster's of the same name, left behind, is not touched.
func (r clusters) deleteUsageHistory(ctx context.Context, obj *cloudv1alpha1.Cluster) error {
	cm := &corev1.ConfigMap{}
	err := r.c.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: usageHistoryName(obj.Name)}, cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return translate(err)
	}
	if cm.Labels[LabelID] != obj.Labels[LabelID] {
		return nil
	}
	if err := r.c.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the recorded usage: %w", translate(err))
	}
	return nil
}

// SetUsageHistory writes the cluster's recorded usage into its ConfigMap,
// owned by the Cluster object so that it goes with it.
func (r clusters) SetUsageHistory(ctx context.Context, tenantID, id string, history []byte) error {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return translate(err)
	}
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: obj.Namespace, Name: usageHistoryName(obj.Name),
			Labels: map[string]string{LabelTenant: tenantID, LabelID: id, LabelUsageHistory: "true"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: cloudv1alpha1.GroupVersion.String(), Kind: "Cluster",
				Name: obj.Name, UID: obj.UID}}},
		BinaryData: map[string][]byte{usageHistoryKey: history},
	}
	return translate(kube.Apply(ctx, r.c, cm))
}

// find is the cluster of the tenant with the row ID.
func (r clusters) find(ctx context.Context, tenantID, id string) (*cloudv1alpha1.Cluster, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	return byID[*cloudv1alpha1.Cluster](ctx, r.store, &cloudv1alpha1.ClusterList{}, kube.TenantNamespace(tenantID), id)
}

// network is the Network object of the row's network: ErrNotFound when
// there is none, ErrInvalid when it is another tenant's.
func (r clusters) network(ctx context.Context, c *model.Cluster) (*cloudv1alpha1.Network, error) {
	network, err := byID[*cloudv1alpha1.Network](ctx, r.store, &cloudv1alpha1.NetworkList{}, "", c.NetworkID)
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	if tenant := network.Labels[LabelTenant]; tenant != c.TenantID {
		return nil, fmt.Errorf("%w: network %s belongs to tenant %s, cluster to tenant %s",
			repository.ErrInvalid, network.Name, tenant, c.TenantID)
	}
	return network, nil
}

// Create stores a cluster as a Cluster object owned by its network, and
// claims the network: the network must not be DELETING, and its etag
// changes. DeleteNetwork marks the network DELETING with the etag it read
// before it checked for clusters, so of a create and a delete that race
// exactly one wins.
//
// Without a transaction the order does the work: the cluster is stored
// first, then the network is claimed. A delete that looked for clusters
// before this one was stored still holds the old etag and is refused; a
// delete that already won leaves the network DELETING (or gone), which
// the claim sees, and the cluster is taken back.
func (r clusters) Create(ctx context.Context, c *model.Cluster) error {
	network, err := r.network(ctx, c)
	if err != nil {
		return err
	}
	if err := onlyAWS(c.Provider); err != nil {
		return err
	}
	if networkIsDeleting(network) {
		return networkDeletingError(network)
	}
	// The network's account is the cluster's.
	c.CloudAccountID = network.Labels[LabelAccountID]
	if err := prepare(&c.Resource); err != nil {
		return err
	}
	reserved, err := r.reserveEKSNameClaim(ctx, c, network, nil)
	if err != nil {
		return err
	}
	obj := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: network.Namespace, Name: c.Name}}
	if err := r.fill(c, network.Name, obj); err != nil {
		return r.dropReservation(ctx, reserved, err)
	}
	if err := owner.Set(network, obj, r.c.Scheme()); err != nil {
		return r.dropReservation(ctx, reserved, invalid(err))
	}
	keys := clusterKeys(c)
	if err := r.claimAll(ctx, obj.Namespace, kindCluster, c.ID, keys); err != nil {
		return r.dropReservation(ctx, reserved, err)
	}
	takeBack := func(ctx context.Context) error { return r.takeBack(ctx, obj, keys, reserved) }
	if err := r.insert(ctx, obj, func() { r.setStatus(obj, c.Status, c.State, &obj.Generation) }); err != nil {
		return r.insertFailed(ctx, c, obj, keys, reserved, err)
	}
	if err := r.claimNetwork(ctx, c.TenantID, c.NetworkID); err != nil {
		if terr := takeBack(ctx); terr != nil && !errors.Is(terr, repository.ErrNotFound) {
			return errors.Join(err, fmt.Errorf("take the cluster back: %w", terr))
		}
		return err
	}
	r.created(takeBack)
	return nil
}

// rollbackTimeout bounds the cleanup of a failed create: it runs detached
// from the caller's context, which is often the very reason the create failed.
const rollbackTimeout = 10 * time.Second

// rollbackContext is a context for a rollback step: not cancelled with ctx,
// but time-limited.
func rollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
}

// dropReservation releases the reservation of an attempt that stored nothing
// and returns err, with the failure to release joined to it.
func (r clusters) dropReservation(ctx context.Context, reserved *corev1.ConfigMap, err error) error {
	ctx, cancel := rollbackContext(ctx)
	defer cancel()
	if rerr := r.releaseAttemptReservation(ctx, reserved); rerr != nil {
		return errors.Join(err, fmt.Errorf("release the physical name: %w", rerr))
	}
	return err
}

// releaseKeys deletes the claims of keys; a claim that stays is reported (it
// is taken over once it is older than claimGrace).
func (r clusters) releaseKeys(ctx context.Context, namespace string, keys ...string) error {
	var errs []error
	for _, key := range keys {
		err := r.c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: claimName(key)}})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("release claim %s: %w", key, translate(err)))
		}
	}
	return errors.Join(errs...)
}

// takeBack deletes the cluster, and only once it is confirmed gone (or held
// by nothing but the garbage collector's orphan finalizer, same UID) lets go
// of what this attempt took: its request claim and its reservation. An
// unconfirmed rollback keeps the reservation (fail closed). It runs detached
// from ctx's cancellation.
func (r clusters) takeBack(ctx context.Context, obj *cloudv1alpha1.Cluster, keys []string, reserved *corev1.ConfigMap) error {
	ctx, cancel := rollbackContext(ctx)
	defer cancel()
	err := r.c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationOrphan))
	if err != nil && !apierrors.IsNotFound(err) {
		return translate(err)
	}
	if !r.rolledBack(ctx, obj) {
		return translate(err)
	}
	var errs []error
	if kerr := r.releaseKeys(ctx, obj.Namespace, keys...); kerr != nil {
		errs = append(errs, kerr)
	}
	if rerr := r.releaseAttemptReservation(ctx, reserved); rerr != nil {
		errs = append(errs, fmt.Errorf("release the physical name: %w", rerr))
	}
	if len(errs) > 0 {
		return errors.Join(append([]error{translate(err)}, errs...)...)
	}
	return translate(err)
}

// rolledBack reports whether the cluster this attempt stored is gone: not
// found, or the same object (UID) held only by the garbage collector's orphan
// finalizer after the orphan-propagation delete.
func (r clusters) rolledBack(ctx context.Context, obj *cloudv1alpha1.Cluster) bool {
	remaining := &cloudv1alpha1.Cluster{}
	err := r.c.Get(ctx, client.ObjectKeyFromObject(obj), remaining)
	if apierrors.IsNotFound(err) {
		return true
	}
	return err == nil && onlyCollectorHolds(remaining, obj)
}

// insertFailed is the end of a create whose insert failed. A name collision is
// another create's cluster: an identical retry (same request ID, network and
// payload) that won gets its cluster returned as the idempotent result, and
// nothing the winner owns (request claim, reservation) is released. Any other
// failure releases the claims only when no cluster of this attempt remains; a
// cluster of this attempt that did get stored (the failure may be the
// caller's context ending after the write) is taken back, so that the outcome
// is what the caller is told.
func (r clusters) insertFailed(ctx context.Context, c *model.Cluster, obj *cloudv1alpha1.Cluster, keys []string,
	reserved *corev1.ConfigMap, err error) error {
	if errors.Is(err, repository.ErrAlreadyExists) {
		winner, werr := r.winner(ctx, c, obj)
		switch {
		case werr == nil && winner != nil:
			if gerr := r.stillStands(ctx, c, winner); gerr != nil {
				return gerr
			}
			*c = *winner
			return nil
		case werr == nil:
			// The name is another request's: its claims are not ours, except the
			// request claim, which only this attempt (a different ID) took. The
			// reservation is this attempt's own (another region or account's
			// cluster of the same name holds the object name, not the physical one).
			if rerr := r.dropReservation(ctx, reserved, nil); rerr != nil {
				err = errors.Join(err, rerr)
			}
			if existing := r.holderOf(ctx, obj.Namespace, keys); existing == "" || existing == c.ID {
				rctx, cancel := rollbackContext(ctx)
				defer cancel()
				if kerr := r.releaseKeys(rctx, obj.Namespace, keys...); kerr != nil {
					return errors.Join(err, kerr)
				}
			}
		}
		return err
	}
	// The create may have stored the cluster before failing: only a proven
	// absence frees the claims.
	rctx, cancel := rollbackContext(ctx)
	defer cancel()
	probe := &cloudv1alpha1.Cluster{}
	perr := r.c.Get(rctx, client.ObjectKeyFromObject(obj), probe)
	switch {
	case apierrors.IsNotFound(perr):
		var errs []error
		if kerr := r.releaseKeys(rctx, obj.Namespace, keys...); kerr != nil {
			errs = append(errs, kerr)
		}
		if rerr := r.releaseAttemptReservation(rctx, reserved); rerr != nil {
			errs = append(errs, fmt.Errorf("release the physical name: %w", rerr))
		}
		return errors.Join(append([]error{err}, errs...)...)
	case perr == nil && probe.Labels[LabelID] == c.ID:
		// Stored by this attempt although it failed: take it back.
		stored := obj.DeepCopy()
		stored.UID = probe.UID
		if terr := r.takeBack(ctx, stored, keys, reserved); terr != nil && !errors.Is(terr, repository.ErrNotFound) {
			return errors.Join(err, fmt.Errorf("take the cluster back: %w", terr))
		}
	case perr != nil:
		return errors.Join(err, fmt.Errorf("check whether the cluster was stored: %w", translate(perr)))
	}
	return err
}

// winner is the cluster that already holds obj's name when it is the same
// create (request ID, network, operation and payload); nil when it is not.
func (r clusters) winner(ctx context.Context, c *model.Cluster, obj *cloudv1alpha1.Cluster) (*model.Cluster, error) {
	if c.CreateRequestID == nil {
		return nil, nil
	}
	have := &cloudv1alpha1.Cluster{}
	if err := r.c.Get(ctx, client.ObjectKeyFromObject(obj), have); err != nil {
		return nil, translate(err)
	}
	row, err := r.row(have)
	if err != nil {
		return nil, err
	}
	if row.TenantID != c.TenantID || row.CreateRequestID == nil || *row.CreateRequestID != *c.CreateRequestID ||
		row.Imported != c.Imported || row.NetworkID != c.NetworkID {
		return nil, nil
	}
	want, err1 := clusterCreateHash(c)
	got, err2 := clusterCreateHash(row)
	if err := errors.Join(err1, err2); err != nil {
		return nil, err
	}
	if want != got {
		return nil, nil
	}
	return row, nil
}

// stillStands confirms that the winner of a name collision is still there and
// is not being taken back (its network claim failing, or being deleted): an
// idempotent success must not name a cluster that is rolled back right after.
func (r clusters) stillStands(ctx context.Context, c *model.Cluster, winner *model.Cluster) error {
	live, err := r.find(ctx, winner.TenantID, winner.ID)
	if errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("%w: the cluster of this request was rolled back, retry the create", repository.ErrConflict)
	}
	if err != nil {
		return err
	}
	if !live.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: the cluster of this request is being taken back, retry the create", repository.ErrConflict)
	}
	network, err := r.network(ctx, c)
	if err != nil {
		return err
	}
	if networkIsDeleting(network) {
		return networkDeletingError(network)
	}
	return nil
}

// holderOf is the holder of the first of keys that has a claim, "" when none.
func (r clusters) holderOf(ctx context.Context, namespace string, keys []string) string {
	for _, key := range keys {
		if id, err := r.holder(ctx, namespace, key); err == nil {
			return id
		}
	}
	return ""
}

func networkIsDeleting(n *cloudv1alpha1.Network) bool {
	return metaOf(n).State == networkDeleting || !n.DeletionTimestamp.IsZero()
}

// ErrNetworkDeleting is wrapped by the ErrConflict of a create in a network
// that is being deleted; other conflicts (a write that kept changing, an
// account being deleted) are not it.
var ErrNetworkDeleting = errors.New("network is being deleted")

func networkDeletingError(n *cloudv1alpha1.Network) error {
	return fmt.Errorf("%w: %w: network %s", repository.ErrConflict, ErrNetworkDeleting, n.Name)
}

// claimNetwork gives the network a new etag unless it is DELETING or has a
// deletionTimestamp (ErrConflict) or is gone (ErrNotFound). It is not
// conditional on an etag read before: two creates in one network, or the loop
// touching the network meanwhile, must not fail each other.
func (r clusters) claimNetwork(ctx context.Context, tenantID, id string) error {
	err := translate(retry(func() error {
		network, err := networks(r).find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if networkIsDeleting(network) {
			return networkDeletingError(network)
		}
		m := metaOf(network)
		m.ETag = newETag()
		if err := setMeta(network, m); err != nil {
			return err
		}
		return r.c.Update(ctx, network)
	}))
	if errors.Is(err, repository.ErrNotFound) {
		return fmt.Errorf("network: %w", err)
	}
	return err
}

func (r clusters) Get(ctx context.Context, tenantID, id string) (*model.Cluster, error) {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	row, err := r.row(obj)
	if err != nil {
		return nil, err
	}
	// Up to 128 KiB: only a single-cluster read carries it, never a List.
	row.PolicySnapshot = model.JSON(obj.Status.PolicySnapshot)
	return row, nil
}

func (r clusters) GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Cluster, error) {
	if !labelValue(tenantID) {
		return nil, repository.ErrNotFound
	}
	id, err := r.holder(ctx, kube.TenantNamespace(tenantID), "cluster-request/"+requestID)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, tenantID, id)
}

// all lists the clusters with labels in namespace, ordered by ID (oldest
// first).
func (r clusters) all(ctx context.Context, namespace string, labels map[string]string) ([]model.Cluster, error) {
	objects, err := items[*cloudv1alpha1.Cluster](ctx, r.store, &cloudv1alpha1.ClusterList{}, namespace, labels)
	if err != nil {
		return nil, err
	}
	out := make([]model.Cluster, 0, len(objects))
	for _, obj := range objects {
		if obj.Labels[LabelID] == "" {
			continue // not a row: an object the store did not write
		}
		c, err := r.row(obj)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r clusters) List(ctx context.Context, tenantID string, p repository.Page) ([]model.Cluster, error) {
	if p.Limit <= 0 {
		return nil, fmt.Errorf("page limit must be positive, got %d", p.Limit)
	}
	rows, err := r.all(ctx, kube.TenantNamespace(tenantID), map[string]string{LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	return page(rows, func(c model.Cluster) string { return c.ID }, p)
}

func (r clusters) ListByNetwork(ctx context.Context, networkID string) ([]model.Cluster, error) {
	return r.all(ctx, "", map[string]string{LabelNetworkID: networkID})
}

// ListByName returns the tenant's cluster of the name: at most one, the
// name is the object's.
func (r clusters) ListByName(ctx context.Context, tenantID, name string) ([]model.Cluster, error) {
	rows, err := r.all(ctx, kube.TenantNamespace(tenantID), map[string]string{LabelTenant: tenantID})
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, c := range rows {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out, nil
}

// Update writes the row if the stored etag is c's, and gives c a new one
// (networks.Update).
func (r clusters) Update(ctx context.Context, c *model.Cluster) error {
	return r.write(ctx, c, true)
}

// Record writes what the controller found out (Repository docs): Update
// without a new etag.
func (r clusters) Record(ctx context.Context, c *model.Cluster) error {
	return r.write(ctx, c, false)
}

// write stores the row if the stored etag is c's and the row did not
// change since c was read (c.Seen), and the object's spec was not edited
// since (c.SeenGeneration: the edit is not overwritten with the row's older
// spec; ErrConflict, the pass after sees it). With rotate it gives c a new
// etag.
//
// The status acknowledges the object generation this write produced, unless
// an edit is still waiting to be taken up (generation ahead of the observed
// one) and this write does not take it up (a new row generation): that edit
// stays unobserved, for the controller to bump and apply.
//
// The generation the write produces is recorded on the object in the same
// update (AnnotationRowGeneration), so the controller, which may wake between
// this update and the status one, can tell an edit made by this store from
// one made on the object (StoreWrote).
func (r clusters) write(ctx context.Context, c *model.Cluster, rotate bool) error {
	old, updated := c.ETag, c.UpdatedAt
	var obj *cloudv1alpha1.Cluster
	var observed *int64
	var predicted int64
	err := retry(func() error {
		var err error
		observed = nil
		if obj, err = r.find(ctx, c.TenantID, c.ID); err != nil {
			return err
		}
		m := metaOf(obj)
		if m.ETag != old || changedSince(obj, c.Seen) || c.SeenGeneration != 0 && obj.Generation != c.SeenGeneration {
			return repository.ErrConflict
		}
		pending := obj.Status.ObservedGeneration != 0 && obj.Generation > obj.Status.ObservedGeneration && !StoreWrote(obj)
		takesUp := c.Generation > m.Generation
		c.CreatedAt = m.CreatedAt
		if rotate {
			c.ETag, c.UpdatedAt = newETag(), now()
		}
		specBefore := obj.Spec.DeepCopy()
		if err := r.fill(c, obj.Spec.Network.NetworkRef.Name, obj); err != nil {
			return err
		}
		predicted = obj.Generation
		if !equality.Semantic.DeepEqual(*specBefore, obj.Spec) {
			predicted++ // the API server counts a change of the spec
		}
		annotate(obj, map[string]string{AnnotationRowGeneration: strconv.FormatInt(predicted, 10)})
		if err := r.c.Update(ctx, obj); err != nil {
			return err
		}
		if !pending || takesUp {
			g := obj.Generation
			observed = &g
		}
		return nil
	})
	if err != nil {
		c.ETag, c.UpdatedAt = old, updated
		return translate(err)
	}
	if obj.Generation != predicted {
		// The prediction was off (the server did not count a change it
		// looked like): the record must not claim a generation an edit made
		// later would get.
		if err := r.recordGeneration(ctx, c.TenantID, c.ID, obj.Generation); err != nil {
			return translate(err)
		}
	}
	c.Seen, c.SeenGeneration = seen(obj), obj.Generation
	return r.writeStatus(ctx, c.TenantID, c.ID, c.ETag, c.Status, obj, observed)
}

// recordGeneration sets AnnotationRowGeneration of the cluster's object to
// the generation it has.
func (r clusters) recordGeneration(ctx context.Context, tenantID, id string, generation int64) error {
	return retry(func() error {
		obj, err := r.find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if obj.Generation != generation {
			return nil // changed since: that is not this write's to record
		}
		annotate(obj, map[string]string{AnnotationRowGeneration: strconv.FormatInt(generation, 10)})
		return r.c.Update(ctx, obj)
	})
}

// StoreWrote reports whether the object's current generation is the one a
// write of the store produced (Update, Record), not one made on the object
// (kubectl edit). Between that write's update of the object and its status
// update the generation is ahead of the observed one, and the controller
// must not take it for an edit.
func StoreWrote(obj *cloudv1alpha1.Cluster) bool {
	g, err := strconv.ParseInt(obj.Annotations[AnnotationRowGeneration], 10, 64)
	return err == nil && g == obj.Generation
}

// writeStatus is networks.writeStatus for a cluster. observed is what the
// status acknowledges, as write decided (nil: leave it).
func (r clusters) writeStatus(ctx context.Context, tenantID, id, etag string, status model.JSON, obj *cloudv1alpha1.Cluster,
	observed *int64) error {
	return translate(retry(func() error {
		if obj == nil {
			var err error
			if obj, err = r.find(ctx, tenantID, id); err != nil {
				return err
			}
		}
		current := obj
		obj = nil // read again on a retry
		m := metaOf(current)
		if m.ETag != etag {
			return nil
		}
		before := current.Status.DeepCopy()
		r.setStatus(current, status, m.State, observed)
		if equality.Semantic.DeepEqual(before, &current.Status) {
			return nil
		}
		return r.c.Status().Update(ctx, current)
	}))
}

// SetStatus writes the status only, and only while the row has etag; the
// etag, the update time and the observed generation stay.
func (r clusters) SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error {
	return translate(retry(func() error {
		obj, err := r.find(ctx, tenantID, id)
		if err != nil {
			return err
		}
		m := metaOf(obj)
		if m.ETag != etag {
			return repository.ErrConflict
		}
		before := obj.Status.DeepCopy()
		r.setStatus(obj, status, m.State, nil)
		if equality.Semantic.DeepEqual(before, &obj.Status) {
			return nil
		}
		return r.c.Status().Update(ctx, obj)
	}))
}

// Delete removes the cluster; ErrInUse while it has components (they are
// disabled first, there is no cascade).
func (r clusters) Delete(ctx context.Context, tenantID, id string) error {
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		return err
	}
	for _, kind := range []model.OwnerKind{model.OwnerCluster, model.OwnerClusterComponents} {
		records, err := r.ManagedResources().ListByOwner(ctx, kind, id)
		if err != nil {
			return err
		}
		if len(records) != 0 {
			return fmt.Errorf("%w: cluster inventory must be finalized before releasing its physical name", repository.ErrInUse)
		}
	}
	enabled, err := items[*cloudv1alpha1.Component](ctx, r.store, &cloudv1alpha1.ComponentList{}, obj.Namespace,
		map[string]string{LabelClusterID: id})
	if err != nil {
		return err
	}
	if len(enabled) > 0 {
		names := make([]string, 0, len(enabled))
		for _, c := range enabled {
			names = append(names, c.Spec.Component)
		}
		sort.Strings(names)
		return fmt.Errorf("%w: cluster %s has components %v", repository.ErrInUse, obj.Name, names)
	}
	row, err := r.row(obj)
	if err != nil {
		return err
	}
	// The inventory is finalized (the cloud side is gone): mark the
	// reservation releasing before the CR goes, so a crash or a failed
	// release after it does not keep the name reserved forever.
	if err := r.markEKSNameReleasing(ctx, row, time.Now()); err != nil {
		return err
	}
	// The recorded usage is a ConfigMap owned by the Cluster; the Cluster is
	// removed with orphan propagation, so the collector would never delete it.
	if err := r.deleteUsageHistory(ctx, obj); err != nil {
		return err
	}
	// Dropping the last finalizer on a deleting CR removes it immediately;
	// remove's subsequent Delete then returns NotFound. The inventory checks
	// above and the fresh absence check below still gate reservation release.
	if err := r.remove(ctx, obj); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	remaining := &cloudv1alpha1.Cluster{}
	if err := r.c.Get(ctx, client.ObjectKeyFromObject(obj), remaining); err == nil {
		if !onlyCollectorHolds(remaining, obj) {
			return fmt.Errorf("%w: cluster still has finalizers; physical name reservation retained", repository.ErrConflict)
		}
	} else if !apierrors.IsNotFound(err) {
		return translate(err)
	}
	if err := r.releaseEKSName(ctx, row); err != nil {
		return err
	}
	r.release(ctx, obj.Namespace, clusterKeys(row)...)
	return nil
}

// onlyCollectorHolds reports whether remaining is the object remove just
// deleted, held by nothing but the garbage collector's orphan finalizer.
// remove deletes with orphan propagation, so the API server adds that
// finalizer and drops the object once the collector has run, a moment
// later: no pass would see the object again to release its reservation.
// Any other finalizer still retains the reservation.
func onlyCollectorHolds(remaining, deleted client.Object) bool {
	if remaining.GetUID() != deleted.GetUID() || remaining.GetDeletionTimestamp().IsZero() {
		return false
	}
	for _, f := range remaining.GetFinalizers() {
		if f != metav1.FinalizerOrphanDependents {
			return false
		}
	}
	return true
}
