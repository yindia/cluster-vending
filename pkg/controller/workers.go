package controller

import "sigs.k8s.io/controller-runtime/pkg/controller"

// objectOptions are the options of the controllers of one object each
// (CloudAccounts, Networks, Clusters): Deps.Workers reconciles at once,
// so one slow pass does not hold up the other objects of its kind. The
// work queue never hands one object to two workers at the same time.
//
// The release, release channel, release hold and component definition
// controllers keep one worker: each pass lists every object of its kind,
// so passes in parallel would only race on the same writes. The
// deprecation scans and recommendation samples keep one too: they reach
// into workload clusters on a schedule of minutes to hours, and one at a
// time bounds what they cost the controller.
func objectOptions(d Deps) controller.Options {
	return controller.Options{MaxConcurrentReconciles: max(d.Workers, 1)}
}
