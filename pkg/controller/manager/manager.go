// Package manager is the platform's controller: a kubebuilder project
// (PROJECT, api/, internal/controller/, config/) inside the repository's
// one Go module. Its API is cloud.cluster-vending.io/v1alpha1
// (CloudAccount owns Network owns Cluster).
//
// The controllers (internal/controller) reconcile those objects by
// calling the reconcilers (pkg/reconcile/account, network, cluster) on the
// store that keeps its rows as these objects (pkg/database/cloudstore).
// The API server writes the same objects and leaves reconciling to this
// controller.
//
// kubebuilder scaffolds a cmd/main.go and a Makefile next to PROJECT; this
// project has neither. The binary is the repository's (`cluster-vending
// controller`, cmd/controller.go, which calls Run), and the generators are
// the root Makefile's `controller-apis` target.
package manager

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/yindia/cluster-vending/pkg/catalog"
	"github.com/yindia/cluster-vending/pkg/config"
	reconcilers "github.com/yindia/cluster-vending/pkg/controller"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/management"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/reconcile/account"
	"github.com/yindia/cluster-vending/pkg/telemetry"
)

// leaseName is the leader election Lease of the controller: not the API
// server's, so both can run.
const leaseName = "controller.cloud.cluster-vending.io"

// Options are what the controller manager runs with.
type Options struct {
	// Kubeconfig and KubeContext are how the management cluster is
	// reached (empty: the default loading rules, then in-cluster).
	Kubeconfig, KubeContext string
	// MetricsAddr is the listen address of the Prometheus metrics; "0"
	// turns them off.
	MetricsAddr string
	// ProbeAddr is the listen address of /healthz and /readyz; "0" turns
	// them off.
	ProbeAddr string
	// LeaderElect runs the controllers behind a Lease in LeaseNamespace,
	// so one replica reconciles.
	LeaderElect    bool
	LeaseNamespace string
	// ServiceAccount is the controller's own service account, bound in
	// each tenant namespace to the kubeconfig reader role; nil outside the
	// cluster.
	ServiceAccount *config.ServiceAccountRef
	// InstallProviders creates the Crossplane providers, provider-helm and
	// the activation policy when missing, running as the platform's
	// defaults (crossplaneaws.DefaultProviderRuntime) and as the management
	// role.
	InstallProviders bool
	// InstallCatalog creates the shipped component definitions
	// (pkg/catalog/definitions) that are missing, once the platform's kinds are
	// served; existing ones are never changed.
	InstallCatalog bool
	// RecoverStuckReleases lets the cluster reconcile remove the Helm
	// record of a platform component's stuck revision in the workload
	// cluster (docs/architecture.md#components-and-the-catalog).
	RecoverStuckReleases bool
	// HelmBackoffMax and HelmMaxFailures are how a failed Helm release is
	// retried (--helm-backoff-max, --helm-max-failures); zero: the
	// defaults of pkg/helmrel.
	HelmBackoffMax  time.Duration
	HelmMaxFailures int
	// Recommendations samples running clusters for resource
	// recommendations.
	Recommendations bool
	// AWSAccess is how the controller reaches tenant accounts: the default
	// region of its SDK, the optional source role and external ID
	// (aws-region, aws-source-role-arn, aws-external-id).
	AWSAccess config.AWSAccess
	// NodeGroups is the platform admin's node group layout.
	NodeGroups config.NodeGroups
	// NaaS is the config file's sub-tenant section (config.NaaS).
	NaaS config.NaaS
	// ReconcileWorkers is how many accounts, networks and clusters of each
	// kind are reconciled at once (0: one).
	ReconcileWorkers int
	// AutomaticUpgrades: upgrade automatic clusters to their channel's
	// Kubernetes minor; off, the rollout is planned and reported only.
	AutomaticUpgrades bool
	// UpgradeTimeout is how long an automatic upgrade may take before the
	// rollout counts it as failed (0: config.DefaultUpgradeTimeout).
	UpgradeTimeout time.Duration
	// TeardownTimeout bounds a cluster deletion's teardown (0:
	// reconcile.DefaultTeardownTimeout).
	TeardownTimeout time.Duration
	// AWS is the management role the providers run as, the providers'
	// credentials Secret when there is no IRSA, and whether keys were
	// found in the environment (local testing).
	AWS config.AWS
	// Logger receives the manager's and the controllers' logs. Nil: they
	// are discarded.
	Logger *slog.Logger
}

// Deps are what the controllers work with (reconcile.Deps on the store of
// the platform's kinds, and a client that reads those kinds at the API
// server).
type Deps = reconcilers.Deps

// Setup registers the three controllers and the probes with mgr. It does
// not contact the management cluster.
func Setup(mgr manager.Manager, d Deps) error {
	if err := reconcilers.SetupAll(mgr, d); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("controller health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("controller ready check: %w", err)
	}
	return nil
}

// Run runs the controller manager until ctx ends. It prepares the
// management cluster (pkg/management): the platform's
// CustomResourceDefinitions and, unless told not to, the Crossplane
// providers are installed, and it waits until their kinds are served. The
// API server installs neither providers nor anything of AWS; it waits for
// the controller. The controller's cache also holds the platform's own
// kinds, which it watches.
//
// The controller is what looks at the cloud. Its lookups start from its
// own credentials (the AWS SDK's default chain: IRSA in production, keys
// in the environment for local testing) and always assume the tenant role
// of the account they read (management.WithAWS).
func Run(ctx context.Context, o Options) error {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	options := management.Options{
		Kubeconfig: o.Kubeconfig, KubeContext: o.KubeContext, Logger: logger,
		InstallProviders: o.InstallProviders, ProviderRuntime: providerRuntime(o.AWS), InstallKinds: true,
		LeaderElection:     kube.LeaderElection{Enabled: o.LeaderElect, Namespace: o.LeaseNamespace},
		WatchPlatformKinds: true, LeaseName: leaseName, MetricsAddr: o.MetricsAddr, ProbeAddr: o.ProbeAddr,
	}
	// The store first: it installs the kinds the manager's cache holds.
	store, err := management.ConnectStore(ctx, options)
	if err != nil {
		return err
	}
	mgr, err := management.Connect(ctx, options)
	if err != nil {
		return fmt.Errorf("%w (--install-providers=false skips installing the providers)", err)
	}
	cfg, err := kube.RESTConfig(o.Kubeconfig, o.KubeContext)
	if err != nil {
		return err
	}
	direct, err := kube.New(cfg)
	if err != nil {
		return err
	}
	if o.InstallCatalog {
		// The platform's kinds are served (ConnectStore waited for them).
		// Every replica may try: each write is conditional on what was read.
		res, err := catalog.Install(ctx, direct)
		if err != nil {
			return fmt.Errorf("%w (--install-catalog=false skips installing the component catalog)", err)
		}
		logger.Info("component catalog", "created", res.Created, "updated", res.Updated, "existing", res.Existing,
			"adopted", res.Adopted, "deleted", res.Deleted)
		if len(res.Edited) > 0 {
			logger.Info("component definitions edited by the platform team are kept as they are; delete one and restart to take this build's",
				"definitions", res.Edited)
		}
		if len(res.Foreign) > 0 {
			logger.Warn("component definitions differ from this build's and were not shipped by it: kept; kubectl replace -f pkg/catalog/definitions/<name>.yaml, or delete and restart, to take this build's",
				"definitions", res.Foreign)
		}
	}
	if o.AWS.StaticKeys != nil {
		logger.Warn("AWS keys found in the environment: they are the controller's own credentials (local testing); every lookup still assumes the tenant role of the account it reads")
	}
	// Always this build's renderer, for every resource: releases replace
	// a configurable target later (docs/architecture.md#releases-channels-and-rollouts).
	renderTarget := int64(crossplaneaws.RenderVersion)
	lookups, err := management.NewAWSClient(ctx, o.AWSAccess, "")
	if err != nil {
		return err
	}
	reconciling := management.WithAWSClient(reconcile.Deps{
		Store:                    store,
		Kube:                     mgr.GetClient(), // reads from the informer cache
		KubeReader:               mgr.GetAPIReader(),
		RenderTarget:             renderTarget,
		ProviderCredentials:      management.CredentialsSecret(o.AWS.ProviderCredentials),
		ControllerServiceAccount: serviceAccount(o.ServiceAccount),
		// The catalog and the releases, read at the API server: what the
		// platform installs with every cluster.
		Platform: management.PlatformSource(direct),
		// Workload clusters, reached with their kubeconfig Secrets (read at
		// the API server, in tenant namespaces only): to recover a stuck
		// Helm release of a platform component.
		Workloads:        management.Workloads(direct),
		NodeGroupClients: management.WorkloadClients(direct),
		// The same, to read whether a component's workloads rolled out.
		Rollouts:             management.WorkloadClients(direct),
		SubTenantWorkloads:   management.SubTenantWorkloads(direct),
		RecoverStuckReleases: o.RecoverStuckReleases,
		HelmBackoffMax:       o.HelmBackoffMax,
		HelmMaxFailures:      o.HelmMaxFailures,
		// The same, first in a cluster's deletion: its load balancers and
		// Karpenter's nodes, while their controllers still run.
		Teardowns:         management.Teardowns(direct),
		TeardownTimeout:   o.TeardownTimeout,
		NoRecommendations: !o.Recommendations,
		Logger:            logger,

		NodeGroups: o.NodeGroups,
		NaaS:       o.NaaS,
	}, lookups)
	checkCredentialsSecret(ctx, reconciling, logger)
	// What happens to accounts, networks, clusters and releases is also a
	// Kubernetes event of the object (docs/architecture.md#events-and-conditions).
	reconciling.Events = reconcilers.NewEvents(mgr.GetEventRecorder(reconcilers.EventsName), mgr.GetClient())
	deps := Deps{Direct: direct, Deps: reconciling, Workers: o.ReconcileWorkers, AutomaticUpgrades: o.AutomaticUpgrades,
		UpgradeTimeout: o.UpgradeTimeout}
	if err := Setup(mgr, deps); err != nil {
		return err
	}
	fleet := telemetry.NewFleet(mgr.GetClient())
	if err := ctrlmetrics.Registry.Register(fleet); err != nil {
		return fmt.Errorf("register fleet metrics: %w", err)
	}
	defer ctrlmetrics.Registry.Unregister(fleet)
	if err := mgr.Add(fleet); err != nil {
		return fmt.Errorf("start fleet metrics publisher: %w", err)
	}
	var ready atomic.Bool
	done, err := management.Start(ctx, mgr, &ready)
	if err != nil {
		return err
	}
	logger.Info("controller manager started", "leader_elect", o.LeaderElect, "render_target", renderTarget,
		"reconcile_workers", o.ReconcileWorkers)
	if err := <-done; err != nil {
		return fmt.Errorf("controller manager: %w", err)
	}
	return nil
}

// checkCredentialsSecret logs how the providers authenticate, and a
// credentials Secret that is missing or lacks its key. It does not stop
// the controller: every account reports the same problem on its status
// (account.CheckCredentialsSecret) until the Secret is fixed.
func checkCredentialsSecret(ctx context.Context, d reconcile.Deps, logger *slog.Logger) {
	ref := d.ProviderCredentials
	if ref == nil {
		logger.Info("the Crossplane AWS providers authenticate with IRSA, then assume each account's tenant role")
		return
	}
	logger.Info("the Crossplane AWS providers authenticate with a credentials Secret (no IRSA), then assume each account's tenant role",
		"secret", ref.Namespace+"/"+ref.Name, "key", ref.Key)
	if err := account.CheckCredentialsSecret(ctx, d); err != nil {
		logger.Error("provider credentials Secret: accounts cannot be bootstrapped until it is fixed", "error", err)
	}
}

// providerRuntime is how the controller installs the AWS providers: the
// platform's defaults, running as the management role (IRSA). Other values
// mean managing the provider objects outside the platform
// (--install-providers=false).
func providerRuntime(a config.AWS) crossplaneaws.ProviderRuntime {
	rt := crossplaneaws.DefaultProviderRuntime()
	rt.ManagementRoleARN = a.ManagementRoleARN
	return rt
}

// serviceAccount is the controller's service account as the reconcilers
// take it; nil for none.
func serviceAccount(ref *config.ServiceAccountRef) *reconcile.ServiceAccount {
	if ref == nil {
		return nil
	}
	return &reconcile.ServiceAccount{Namespace: ref.Namespace, Name: ref.Name}
}
