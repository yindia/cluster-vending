package cloudstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// EKSNameReservationNamespace is intentionally independent of tenant and leader
// election namespaces: every API/controller replica must share the same domain.
// kube-system exists on every management cluster; claims have no cross-namespace
// owner references and are released only by confirmed cluster finalization.
const EKSNameReservationNamespace = metav1.NamespaceSystem

const eksNameClaimLabel = group + "eks-name-reservation"

// ErrEKSNameReserved is safe to return to callers in any tenant.
var ErrEKSNameReserved = errors.New("physical EKS name is reserved or has conflicting ownership in this AWS account and region")

func eksNameKey(accountID, region, name string) string {
	return crossplaneaws.Partition(region) + "/" + accountID + "/" + region + "/" + name
}

func eksClaimName(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "cv-eks-name-" + hex.EncodeToString(digest[:20])
}

func eksNameConflict() error {
	// Never disclose the competing tenant, allocation UUID or credentials.
	return fmt.Errorf("%w: %w", repository.ErrAlreadyExists, ErrEKSNameReserved)
}

func clusterCreateHash(row *model.Cluster) (string, error) {
	var input any
	if len(row.Input) > 0 {
		if err := json.Unmarshal(row.Input, &input); err != nil {
			return "", invalid(err)
		}
	}
	raw, err := json.Marshal([]any{row.NetworkID, input})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (r clusters) physicalAccount(ctx context.Context, network *cloud.Network) (string, error) {
	account := &cloud.CloudAccount{}
	if err := r.c.Get(ctx, client.ObjectKey{Namespace: network.Namespace, Name: network.Spec.AccountRef.Name}, account); err != nil {
		return "", translate(err)
	}
	if account.Spec.AWS == nil || model.ValidateAccount(model.ProviderAWS, account.Spec.AWS.AccountID, "") != nil {
		return "", fmt.Errorf("%w: canonical AWS account identity is unavailable", repository.ErrInvalid)
	}
	return account.Spec.AWS.AccountID, nil
}

// checkLegacyEKSOwners prevents the first new reservation from stealing an
// existing managed/imported/native allocation. Conflicting legacy allocations
// are blocked for explicit operator resolution, never renamed or adopted.
func (r clusters) checkLegacyEKSOwners(ctx context.Context, row *model.Cluster, accountID string, own *cloud.Cluster) error {
	all := &cloud.ClusterList{}
	if err := r.c.List(ctx, all); err != nil {
		return translate(err)
	}
	for i := range all.Items {
		candidate := &all.Items[i]
		if candidate.Name != row.Name || candidate.Spec.Region != row.Region {
			continue
		}
		if candidate.Namespace == kube.TenantNamespace(row.TenantID) && candidate.Labels[LabelID] == row.ID {
			continue
		}
		if own != nil && candidate.UID == own.UID && candidate.Namespace == own.Namespace && candidate.Name == own.Name {
			continue // adopting this native management object, not its cloud peer
		}
		network := &cloud.Network{}
		if err := r.c.Get(ctx, client.ObjectKey{Namespace: candidate.Namespace, Name: candidate.Spec.Network.NetworkRef.Name}, network); err != nil {
			return eksNameConflict() // unknown scope is not evidence of absence
		}
		otherAccount, err := r.physicalAccount(ctx, network)
		if err != nil || accountID == otherAccount {
			return eksNameConflict()
		}
	}
	return nil
}

func (r clusters) reserveEKSName(ctx context.Context, row *model.Cluster, network *cloud.Network, own *cloud.Cluster) error {
	_, err := r.reserveEKSNameClaim(ctx, row, network, own)
	return err
}

// reserveEKSNameClaim is reserveEKSName; it also returns the reservation this
// call created, nil when it found an existing one (a retry or another actor's
// that this attempt shares and must never release on its own).
func (r clusters) reserveEKSNameClaim(ctx context.Context, row *model.Cluster, network *cloud.Network, own *cloud.Cluster) (*corev1.ConfigMap, error) {
	accountID, err := r.physicalAccount(ctx, network)
	if err != nil {
		return nil, err
	}
	key := eksNameKey(accountID, row.Region, row.Name)
	existing := &corev1.ConfigMap{}
	lookupErr := r.c.Get(ctx, client.ObjectKey{Namespace: EKSNameReservationNamespace, Name: eksClaimName(key)}, existing)
	if lookupErr == nil {
		if !validEKSClaim(existing, key, row.TenantID) {
			return nil, eksNameConflict()
		}
		if own != nil && existing.Data["clusterUID"] != "" && existing.Data["clusterUID"] != string(own.UID) {
			return nil, eksNameConflict()
		}
		if own != nil && own.Labels[LabelID] == row.ID && existing.Data["clusterID"] != row.ID {
			return nil, eksNameConflict()
		}
		// Established ownership requires only direct gets. All competing native
		// and RPC writers must reserve the same name before cloud application;
		// rescanning the whole management inventory on every pass is unnecessary.
		if own != nil && existing.Data["clusterID"] == row.ID && existing.Data["clusterUID"] == string(own.UID) &&
			own.Labels[LabelID] == row.ID && own.Name == row.Name && own.Namespace == kube.TenantNamespace(row.TenantID) {
			return nil, nil
		}
	} else if !apierrors.IsNotFound(lookupErr) {
		return nil, translate(lookupErr)
	}
	if err := r.checkLegacyEKSOwners(ctx, row, accountID, own); err != nil {
		return nil, err
	}
	hash, err := clusterCreateHash(row)
	if err != nil {
		return nil, err
	}
	request := ""
	if row.CreateRequestID != nil {
		request = *row.CreateRequestID
	}
	desired := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: EKSNameReservationNamespace, Name: eksClaimName(key), Labels: map[string]string{eksNameClaimLabel: "true"}},
		Data: map[string]string{"key": key, "tenant": row.TenantID, "clusterID": row.ID, "requestID": request, "requestHash": hash}}
	if own != nil {
		desired.Data["clusterUID"] = string(own.UID)
	}
	var created *corev1.ConfigMap
	if err := r.c.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, translate(err)
		}
		have := &corev1.ConfigMap{}
		if err := r.c.Get(ctx, client.ObjectKeyFromObject(desired), have); err != nil {
			return nil, translate(err)
		}
		if !validEKSClaim(have, key, row.TenantID) {
			return nil, eksNameConflict()
		}
		if own != nil && have.Data["clusterUID"] != "" && have.Data["clusterUID"] != string(own.UID) {
			return nil, eksNameConflict()
		}
		if have.Data["clusterID"] != row.ID {
			// A crash after reservation but before CR creation can be resumed only
			// by the exact request. No age-based takeover of a cloud identity.
			nativeRetry := own != nil && own.UID != "" && have.Data["clusterUID"] == string(own.UID) && own.Labels[LabelID] == ""
			if !nativeRetry {
				if have.Data["clusterUID"] != "" || request == "" || have.Data["requestID"] != request || have.Data["requestHash"] != hash {
					return nil, eksNameConflict()
				}
				_, lookupErr := r.find(ctx, row.TenantID, have.Data["clusterID"])
				if !errors.Is(lookupErr, repository.ErrNotFound) {
					return nil, eksNameConflict()
				}
			}
			row.ID = have.Data["clusterID"]
		}
		if own != nil && own.UID != "" && have.Data["clusterUID"] == "" {
			have.Data["clusterUID"] = string(own.UID)
			if err := r.c.Update(ctx, have); err != nil {
				return nil, translate(err)
			}
		}
	} else {
		created = desired.DeepCopy()
	}
	if err := r.checkLegacyEKSOwners(ctx, row, accountID, own); err != nil {
		if created != nil {
			// Nothing was stored under it yet: the reservation just taken is this
			// attempt's alone.
			_ = r.releaseAttemptReservation(ctx, created)
		}
		return nil, err
	}
	return created, nil
}

// releaseAttemptReservation deletes the reservation this attempt created, but
// only while it is still that object (UID, resourceVersion preconditions) and
// still says what the attempt wrote (tenant, cluster ID, request ID and hash).
// Anything else belongs to someone else and stays.
func (r clusters) releaseAttemptReservation(ctx context.Context, mine *corev1.ConfigMap) error {
	have := &corev1.ConfigMap{}
	if err := r.c.Get(ctx, client.ObjectKeyFromObject(mine), have); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return translate(err)
	}
	if have.UID != mine.UID || !validEKSClaim(have, mine.Data["key"], mine.Data["tenant"]) {
		return nil
	}
	for _, k := range []string{"clusterID", "requestID", "requestHash", "clusterUID"} {
		if have.Data[k] != mine.Data[k] {
			return nil
		}
	}
	err := r.c.Delete(ctx, have, client.Preconditions{UID: &have.UID, ResourceVersion: &have.ResourceVersion})
	if err != nil && !apierrors.IsNotFound(err) {
		return translate(err)
	}
	return nil
}

func validEKSClaim(claim *corev1.ConfigMap, key, tenant string) bool {
	return claim.DeletionTimestamp.IsZero() && len(claim.OwnerReferences) == 0 && claim.Labels[eksNameClaimLabel] == "true" &&
		claim.Data["clusterID"] != "" && claim.Data["key"] == key && claim.Data["tenant"] == tenant
}

// EnsureEKSNameReservation covers legacy/native/imported registrations before
// reconciliation can write cloud resources. Non-Kubernetes test stores have no
// cloud identity domain; the production store is always this Kubernetes store.
func EnsureEKSNameReservation(ctx context.Context, s repository.Store, row *model.Cluster) error {
	st, ok := s.(store)
	if !ok {
		return nil
	}
	r := clusters{st}
	network, err := r.network(ctx, row)
	if err != nil {
		return err
	}
	obj, err := r.find(ctx, row.TenantID, row.ID)
	if err != nil {
		return err
	}
	return r.reserveEKSName(ctx, row, network, obj)
}

func (r clusters) releaseEKSName(ctx context.Context, row *model.Cluster) error {
	claims := &corev1.ConfigMapList{}
	if err := r.c.List(ctx, claims, client.InNamespace(EKSNameReservationNamespace), client.MatchingLabels{eksNameClaimLabel: "true"}); err != nil {
		return translate(err)
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Data["tenant"] == row.TenantID && claim.Data["clusterID"] == row.ID {
			if err := r.c.Delete(ctx, claim, client.Preconditions{UID: &claim.UID, ResourceVersion: &claim.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
				return translate(err)
			}
		}
	}
	return nil
}
