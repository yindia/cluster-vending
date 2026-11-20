package cloudstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
)

// honorCancel makes every call of k fail once its context is done, as a real
// client does, and lets after run when the named call is made.
func honorCancel(k client.Client, hook func(ctx context.Context, op string, obj client.Object) error) client.Client {
	check := func(ctx context.Context, op string, obj client.Object) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hook != nil {
			return hook(ctx, op, obj)
		}
		return nil
	}
	return interceptor.NewClient(k.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := check(ctx, "create-before", obj); err != nil {
				return err
			}
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			return check(ctx, "create-after", obj)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := check(ctx, "update", obj); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
}

func claimsLeft(t *testing.T, k client.Client) int {
	t.Helper()
	list := &corev1.ConfigMapList{}
	if err := k.List(t.Context(), list, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

func TestCreateCancelledMidCreateRollsEverythingBack(t *testing.T) {
	steps := map[string]struct {
		op   string
		kind client.Object
	}{
		"cluster stored, then cancelled": {"create-after", &cloud.Cluster{}},
		"cancelled before the cluster":   {"create-before", &cloud.Cluster{}},
		"cancelled claiming the network": {"update", &cloud.Network{}},
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			baseline := claimsLeft(t, w.k)
			k := honorCancel(w.k, func(ctx context.Context, op string, obj client.Object) error {
				if op == step.op && fmtKind(obj) == fmtKind(step.kind) {
					cancel()
					return ctx.Err()
				}
				return nil
			})
			rid := uuid.NewString()
			row := reservationCluster(t, w.network, "cancelled")
			row.CreateRequestID = &rid
			err := cloudstore.New(k).Clusters().Create(ctx, row)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("create error = %v, want context.Canceled", err)
			}
			if clusterExists(t, w.k, "cancelled") {
				t.Fatal("a create the caller was told failed left its cluster stored")
			}
			if n := len(reservations(t, w.k)); n != 0 {
				t.Fatalf("reservation kept: %d", n)
			}
			if n := claimsLeft(t, w.k); n != baseline {
				t.Fatalf("request claim kept: %d configmaps, had %d", n, baseline)
			}
			retry := reservationCluster(t, w.network, "cancelled")
			if err := w.s.Clusters().Create(t.Context(), retry); err != nil {
				t.Fatalf("retry after the cancelled create: %v", err)
			}
		})
	}
}

func fmtKind(o client.Object) string {
	switch o.(type) {
	case *cloud.Cluster:
		return "cluster"
	case *cloud.Network:
		return "network"
	}
	return "other"
}
