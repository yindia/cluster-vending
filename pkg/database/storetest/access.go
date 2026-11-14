package storetest

import (
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// SetAccess writes access as the status of the account (by row ID) of a
// store over c (NewClient), as the controller's access check does.
func SetAccess(t *testing.T, c client.Client, tenantID, id string, access *model.AccountAccess) {
	t.Helper()
	list := &cloudv1alpha1.CloudAccountList{}
	if err := c.List(t.Context(), list, client.InNamespace(kube.TenantNamespace(tenantID)),
		client.MatchingLabels{cloudstore.LabelID: id}); err != nil || len(list.Items) != 1 {
		t.Fatalf("account %s: %d found (%v)", id, len(list.Items), err)
	}
	obj := &list.Items[0]
	obj.Status.Access = cloudstore.AccessStatus(access)
	if err := c.Status().Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
}

// Verified is an access check that passed in regions just now.
func Verified(regions ...string) *model.AccountAccess {
	now := time.Now().UTC().Truncate(time.Second)
	a := &model.AccountAccess{Verified: true, CheckedAt: now, AttemptedAt: now}
	for _, r := range regions {
		a.Regions = append(a.Regions, model.AccessResult{Region: r, OK: true})
	}
	return a
}
