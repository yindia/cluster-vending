package cloudstore

import (
	"context"
	"errors"
	"fmt"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/owner"
)

// errNotThisStore is what the Adopt functions return for another store.
var errNotThisStore = errors.New("only the store on the cloud.cluster-vending.io kinds can adopt an object")

// AdoptNetwork makes obj, a Network someone created without the API (it
// has no row ID), the row n: what Networks().Create would have stored for
// it, written onto the object that exists. The typed spec stays exactly
// as it was written: it is the desired state, the layout it leaves to
// the defaults is resolved again whenever the network is rendered, and
// the kind does not let a layout change. n gets its ID and etag; obj is
// updated in place.
func AdoptNetwork(ctx context.Context, s repository.Store, obj *cloudv1alpha1.Network, n *model.Network) error {
	st, ok := s.(store)
	if !ok {
		return errNotThisStore
	}
	r := networks{st}
	if err := onlyAWS(n.Provider); err != nil {
		return err
	}
	account, err := r.account(ctx, n)
	if err != nil {
		return err
	}
	if err := prepare(&n.Resource); err != nil {
		return err
	}
	label(obj, map[string]string{LabelID: n.ID, LabelTenant: n.TenantID, LabelAccountID: n.CloudAccountID})
	annotate(obj, map[string]string{AnnotationInput: string(n.Input)})
	m := resourceMeta(&n.Resource)
	m.Provider = string(n.Provider)
	if err := setMeta(obj, m); err != nil {
		return err
	}
	if err := owner.Set(account, obj, r.c.Scheme()); err != nil {
		return invalid(err)
	}
	if err := r.inheritPause(ctx, obj); err != nil {
		return err
	}
	if err := r.c.Update(ctx, obj); err != nil {
		return translate(err)
	}
	r.setStatus(obj, n.Status, n.State)
	return translate(r.c.Status().Update(ctx, obj))
}

// AdoptCluster makes obj, a Cluster someone created without the API, the
// row c: what Clusters().Create would have stored for it, written onto
// the object that exists, and its network claimed the same way. Unlike a
// network's, the spec is written too: a cluster is rendered from its
// resolved spec (zones, default add-ons, versions), which only the row
// has.
func AdoptCluster(ctx context.Context, s repository.Store, obj *cloudv1alpha1.Cluster, c *model.Cluster) error {
	st, ok := s.(store)
	if !ok {
		return errNotThisStore
	}
	r := clusters{st}
	network, err := r.network(ctx, c)
	if err != nil {
		return err
	}
	if err := onlyAWS(c.Provider); err != nil {
		return err
	}
	if metaOf(network).State == networkDeleting {
		return fmt.Errorf("%w: network %s is being deleted", repository.ErrConflict, network.Name)
	}
	c.CloudAccountID = network.Labels[LabelAccountID]
	if err := prepare(&c.Resource); err != nil {
		return err
	}
	if err := r.fill(c, network.Name, obj); err != nil {
		return err
	}
	if err := owner.Set(network, obj, r.c.Scheme()); err != nil {
		return invalid(err)
	}
	if err := r.inheritPause(ctx, obj); err != nil {
		return err
	}
	if err := r.c.Update(ctx, obj); err != nil {
		return translate(err)
	}
	r.setStatus(obj, c.Status, c.State)
	if err := r.c.Status().Update(ctx, obj); err != nil {
		return translate(err)
	}
	return r.claimNetwork(ctx, c.TenantID, c.NetworkID)
}
