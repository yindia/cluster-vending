// Package controller holds the reconcilers of the platform's kinds. None
// is implemented yet: each Reconcile returns at once. What is in place is
// what the manager needs to know: which kind each reconciler is for and
// which kind it owns (CloudAccount owns Network owns Cluster), so a change
// to a dependent wakes its owner.
package controller

import (
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
)

// SetupAll registers the three reconcilers with mgr.
func SetupAll(mgr ctrl.Manager) error {
	setups := map[string]func(ctrl.Manager) error{
		"cloudaccount": (&CloudAccountReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager,
		"network":      (&NetworkReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager,
		"cluster":      (&ClusterReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager,
	}
	for name, setup := range setups {
		if err := setup(mgr); err != nil {
			return fmt.Errorf("set up the %s controller: %w", name, err)
		}
	}
	return nil
}
