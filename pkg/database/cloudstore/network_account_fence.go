package cloudstore

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
)

const accountFencePending = group + "account-fence-pending"
const accountNetworkFence = group + "network-create-fence"

func activeNetworkAccount(account *cloud.CloudAccount) error {
	if !account.DeletionTimestamp.IsZero() || account.Status.Phase == cloud.CloudAccountPhaseDeleting {
		return fmt.Errorf("%w: cloud account is being deleted", repository.ErrConflict)
	}
	return nil
}

// The Network is visible before this CAS, but unreadable as a provisionable row
// while pending. A delete prepared before the child sees an obsolete account RV;
// a delete that won first makes the CAS refuse the child. No cloud writes can
// start between those operations.
func (r networks) finishAccountFence(ctx context.Context, obj *cloud.Network) error {
	expectedUID := obj.Annotations[accountFencePending]
	if expectedUID == "" {
		return nil
	}
	err := retry(func() error {
		account := &cloud.CloudAccount{}
		if err := r.c.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Spec.AccountRef.Name}, account); err != nil {
			return err
		}
		if string(account.UID) != expectedUID && (account.UID != "" || expectedUID != "legacy-no-uid") {
			return fmt.Errorf("%w: cloud account incarnation changed", repository.ErrConflict)
		}
		if account.Labels[LabelID] != obj.Labels[LabelAccountID] {
			return fmt.Errorf("%w: cloud account allocation changed", repository.ErrConflict)
		}
		if err := activeNetworkAccount(account); err != nil {
			return err
		}
		annotate(account, map[string]string{accountNetworkFence: newETag()})
		return r.c.Update(ctx, account)
	})
	if err != nil {
		return translate(err)
	}
	return translate(retry(func() error {
		current := &cloud.Network{}
		if err := r.c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return err
		}
		if current.Labels[LabelID] != obj.Labels[LabelID] || current.Annotations[accountFencePending] != expectedUID {
			return repository.ErrConflict
		}
		delete(current.Annotations, accountFencePending)
		if err := r.c.Update(ctx, current); err != nil {
			return err
		}
		*obj = *current
		return nil
	}))
}

func markAccountFence(obj *cloud.Network, account *cloud.CloudAccount) {
	uid := string(account.UID)
	if uid == "" { // fake/legacy stores only; real Kubernetes always assigns UIDs
		uid = "legacy-no-uid"
	}
	annotate(obj, map[string]string{accountFencePending: uid})
}

// ResumeNetworkCreate is called only by a mutating create retry. It recovers a
// crash between Network persistence and the account fence without giving Get or
// List side effects. A failed fence never makes the pending row provisionable.
func ResumeNetworkCreate(ctx context.Context, s repository.Store, tenantID, requestID string) error {
	st, ok := s.(store)
	if !ok || requestID == "" {
		return nil
	}
	id, err := st.holder(ctx, kube.TenantNamespace(tenantID), "network-request/"+requestID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	r := networks{st}
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil // reservation may precede the CR; normal create retries it
		}
		return err
	}
	if obj.Annotations[accountFencePending] == "" {
		return nil
	}
	return r.finishAccountFence(ctx, obj)
}

// ResumeNetworkAccountFence is for the mutating reconcile path, including native
// adoption and creates without a request ID. Pure reads remain side-effect free.
func ResumeNetworkAccountFence(ctx context.Context, s repository.Store, tenantID, id string) error {
	st, ok := s.(store)
	if !ok {
		return nil
	}
	r := networks{st}
	obj, err := r.find(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil
		}
		return err
	}
	return r.finishAccountFence(ctx, obj)
}

func (r networks) takeBackPending(ctx context.Context, obj *cloud.Network) error {
	// No controller may apply pending intent. Never strip a concurrent finalizer.
	err := r.c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationOrphan), client.Preconditions{ResourceVersion: &obj.ResourceVersion})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return translate(err)
}
