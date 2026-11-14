package cloudstore

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// AnnotationAccessRequested is when an access check of a CloudAccount was
// last asked for (RFC 3339; TenantService.VerifyCloudAccount). The
// controller runs a check when it is later than the last one tried.
const AnnotationAccessRequested = group + "access-requested"

// RequestAccessCheck implements repository.Tenants: a patch of the one
// annotation, so the row (its etag, its spec) is not written.
func (r tenants) RequestAccessCheck(ctx context.Context, tenantID, id string, at time.Time) error {
	obj, err := byID[*cloudv1alpha1.CloudAccount](ctx, r.store, &cloudv1alpha1.CloudAccountList{}, kube.TenantNamespace(tenantID), id)
	if err != nil {
		return err
	}
	before := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	annotate(obj, map[string]string{AnnotationAccessRequested: at.UTC().Format(time.RFC3339Nano)})
	err = r.c.Patch(ctx, obj, before)
	if apierrors.IsNotFound(err) {
		return repository.ErrNotFound
	}
	return translate(err)
}

// accessRequestedOf is the request time an account carries; zero for none
// or one that does not parse.
func accessRequestedOf(obj *cloudv1alpha1.CloudAccount) time.Time {
	at, err := time.Parse(time.RFC3339Nano, obj.Annotations[AnnotationAccessRequested])
	if err != nil {
		return time.Time{}
	}
	return at
}

// AccessModel is the stored form of an account's typed status.access.
func AccessModel(in *cloudv1alpha1.AccountAccess) *model.AccountAccess {
	if in == nil {
		return nil
	}
	out := &model.AccountAccess{Verified: in.Verified, Stale: in.Stale, PrincipalARN: in.PrincipalARN,
		Reason: in.Reason, Message: in.Message, CheckedAt: timeOf(in.CheckedAt), AttemptedAt: timeOf(in.AttemptedAt)}
	for _, r := range in.Regions {
		out.Regions = append(out.Regions, model.AccessResult(r))
	}
	for _, c := range in.Checks {
		out.Checks = append(out.Checks, model.AccessResult(c))
	}
	return out
}

// AccessStatus is the typed status.access of an account's check.
func AccessStatus(in *model.AccountAccess) *cloudv1alpha1.AccountAccess {
	if in == nil {
		return nil
	}
	out := &cloudv1alpha1.AccountAccess{Verified: in.Verified, Stale: in.Stale, PrincipalARN: in.PrincipalARN,
		Reason: in.Reason, Message: in.Message, CheckedAt: metaTime(in.CheckedAt), AttemptedAt: metaTime(in.AttemptedAt)}
	for _, r := range in.Regions {
		out.Regions = append(out.Regions, cloudv1alpha1.AccessResult(r))
	}
	for _, c := range in.Checks {
		out.Checks = append(out.Checks, cloudv1alpha1.AccessResult(c))
	}
	return out
}

func timeOf(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

func metaTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	m := metav1.NewTime(t)
	return &m
}
