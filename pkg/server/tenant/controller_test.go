package tenant

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// errWrite fails any write to the management cluster.
var errWrite = errors.New("the handler wrote to the management cluster")

// noWrites fails every write the handler would send to the management
// cluster.
func noWrites() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return errWrite },
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return errWrite },
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errWrite
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return errWrite },
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return errWrite
		},
	}
}

// When a controller reconciles accounts, BootstrapTenant stores the
// account and writes nothing to the management cluster: the CloudAccount
// controller applies the namespace and the ProviderConfig (with the
// providers' credentials it is configured with).
func TestBootstrapTenantOnlyStoresWhenAControllerReconciles(t *testing.T) {
	f := newFixture(t, noWrites())
	f.svc.deps.AccountsByController = true
	f.expectNewTenant()
	f.expectPut(account)

	res, err := f.svc.BootstrapTenant(t.Context(), request("acme", account))
	if err != nil {
		t.Fatalf("BootstrapTenant: %v", err)
	}
	if res.GetAccount().GetStatus().GetProviderConfigName() != "aws-0a1b2c3d" {
		t.Errorf("account = %v", res.GetAccount())
	}
	if len(*f.recorded) != 0 {
		t.Errorf("%d objects recorded, want none: nothing was applied", len(*f.recorded))
	}
	if st := res.GetAccount().GetStatus().GetState(); st == cloudv1.CloudAccountState_CLOUD_ACCOUNT_STATE_UNSPECIFIED {
		t.Errorf("state = %s", st)
	}
}
