// Package component implements cloud.v1 ComponentService: the catalog a
// tenant reads, and the components a tenant enables on its clusters.
//
// The handlers validate and store; they install nothing. A Platform
// component is installed with its cluster by the cluster reconcile. A
// tenant's Component is stored PENDING: the controller that installs
// tenant components does not exist yet (docs/design/component-usage.md).
package component

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/structpb"
	"k8s.io/apimachinery/pkg/api/meta"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	core "github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200

	phasePending  = "COMPONENT_PHASE_PENDING"
	phaseDeleting = "COMPONENT_PHASE_DELETING"

	// pendingMessage is the status of a stored component: honest about
	// what does not happen yet.
	pendingMessage = "stored; not installed yet: this build installs platform components only"
	// notEnabled is the message of a catalog component a cluster does not have.
	notEnabled = "not enabled"
	// disabledMessage is the message of a catalog component the platform
	// switched off.
	disabledMessage = "disabled by the platform"
)

// Service implements cloudv1connect.ComponentServiceHandler on the store
// and the catalog. It makes no cloud call and reads no workload cluster.
type Service struct {
	deps reconcile.Deps
	// catalog reads ComponentDefinitions, ReleaseChannels and Releases from
	// the management cluster. Nil: every call answers UNAVAILABLE.
	catalog reconcile.PlatformSource
}

var _ cloudv1connect.ComponentServiceHandler = (*Service)(nil)

// NewService is the component service on d's store, reading the catalog
// and the releases through catalog.
func NewService(d reconcile.Deps, catalog reconcile.PlatformSource) *Service {
	return &Service{deps: d, catalog: catalog}
}

func (s *Service) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.Default()
	}
	return s.deps.Logger
}

// rpcError answers faults and ended contexts with their Connect code.
func rpcError(err error) error {
	if _, ok := fault.As(err); ok || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return service.ToConnect(err)
	}
	return err
}

// entry is one definition of the catalog: as the checks take it, and the
// object it came from.
type entry struct {
	core.Definition
	object cloudv1alpha1.ComponentDefinition
}

// entries reads the catalog, by name. A definition is Valid when the
// controller said so, or, while nothing sets that condition, when it
// passes the definition checks here.
func (s *Service) entries(ctx context.Context) ([]entry, error) {
	if s.catalog == nil {
		return nil, fault.New(fault.Unavailable, "the component catalog is not available")
	}
	objects, err := s.catalog.Definitions(ctx)
	if err != nil {
		s.logger().ErrorContext(ctx, "read the component catalog", "error", err)
		return nil, fault.New(fault.Unavailable, "the component catalog cannot be read")
	}
	out := make([]entry, 0, len(objects))
	for i := range objects {
		d, err := core.DefinitionOf(&objects[i])
		if err != nil {
			continue // unusable: not in the catalog
		}
		if cond := meta.FindStatusCondition(objects[i].Status.Conditions, core.ConditionValid); cond == nil {
			d.Valid = len(core.CheckDefinition(d)) == 0
		}
		out = append(out, entry{Definition: d, object: objects[i]})
	}
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// definitions are the entries as the checks take a catalog.
func definitions(entries []entry) []core.Definition {
	out := make([]core.Definition, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Definition)
	}
	return out
}

// find is the valid entry of name; nil when the catalog has none.
func find(entries []entry, name string) *entry {
	for i := range entries {
		if entries[i].Name == name && entries[i].Valid {
			return &entries[i]
		}
	}
	return nil
}

// configurable reports whether a tenant may set values for d.
func configurable(d core.Definition) bool {
	return !d.IsPlatform() && !d.Internal && d.UserValues != "" && d.ValuesSchema != nil
}

func management(d core.Definition) cloudv1.ComponentManagement {
	if d.IsPlatform() {
		return cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_PLATFORM
	}
	return cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_TENANT
}

// cluster reads the tenant's cluster; nil when the tenant has none of
// that ID.
func (s *Service) cluster(ctx context.Context, tenantID, id string) (*model.Cluster, error) {
	row, err := s.deps.Store.Clusters().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return row, nil
}

func noCluster(tenantID, id string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Sprintf("tenant %s has no cluster %s", tenantID, id))
}

// clusterStatus is the status last recorded for the cluster.
func clusterStatus(row *model.Cluster) *cloudv1.ClusterStatus {
	st := &cloudv1.ClusterStatus{}
	if len(row.Status) > 0 {
		if err := reconcilecluster.Lenient.Unmarshal(row.Status, st); err != nil {
			return &cloudv1.ClusterStatus{}
		}
	}
	return st
}

// minorOf is the Kubernetes minor of a version: "1.37" of "1.37.2".
func minorOf(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// channelOf is the release channel a cluster follows: stable unless its
// spec names another.
func channelOf(spec *cloudv1.ClusterSpec) string {
	switch spec.GetReleaseChannel() {
	case cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA:
		return cloudv1alpha1.ChannelAlpha
	case cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA:
		return cloudv1alpha1.ChannelBeta
	default:
		return cloudv1alpha1.ChannelStable
	}
}

// target is a cluster as the checks take it, with what its release offers.
type target struct {
	cluster  core.Cluster
	offering *core.Offering
	// pins are the release's locked values per component, on the
	// cluster's Kubernetes minor.
	pins map[string]core.Values
}

// targetOf reads what the checks need of the cluster from its row, and
// the release that applies to it from the management cluster: the release
// its platform components were installed from, else what its channel
// offers. No release: a nil offering.
func (s *Service) targetOf(ctx context.Context, row *model.Cluster) (target, error) {
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		return target{}, service.ToConnect(err)
	}
	st := clusterStatus(row)
	version := spec.GetControlPlane().GetKubernetesVersion()
	if version == "" {
		version = st.GetCurrentKubernetesVersion()
	}
	// Also the IAM OIDC provider a taken-over cluster was imported with,
	// which its spec does not say.
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return target{}, service.ToConnect(err)
	}
	t := target{cluster: core.Cluster{
		Name: row.Name, State: strings.TrimPrefix(row.State, "CLUSTER_STATE_"),
		Running:         row.State == reconcilecluster.StateRunning,
		Deleting:        row.State == reconcilecluster.StateDeleting,
		Imported:        row.State == reconcilecluster.StateImported,
		KubernetesMinor: minorOf(version), IRSA: reconcilecluster.HasIRSA(spec, records),
	}}
	for _, a := range spec.GetAws().GetAddons() {
		t.cluster.Addons = append(t.cluster.Addons, a.GetName())
	}
	rel, err := s.release(ctx, st.GetPlatformRelease(), channelOf(spec))
	if err != nil || rel == nil {
		return t, err
	}
	t.offering = &core.Offering{Release: rel.Name, Versions: map[string]string{}}
	t.pins = map[string]core.Values{}
	for _, k := range rel.Spec.Content.Kubernetes {
		if k.Minor != t.cluster.KubernetesMinor {
			continue
		}
		t.offering.Supported = true
		for _, pin := range k.Components {
			t.offering.Versions[pin.Name] = pin.Version
			if values, err := core.PinValues(pin.Values); err == nil && values != nil {
				t.pins[pin.Name] = values
			}
		}
	}
	return t, nil
}

// release is the release that applies: kept (the one the cluster's
// platform components came from) while it exists and is valid, else what
// the channel offers. Nil when there is none.
func (s *Service) release(ctx context.Context, kept, channel string) (*cloudv1alpha1.Release, error) {
	unavailable := func(err error) error {
		s.logger().ErrorContext(ctx, "read the cluster's release", "error", err)
		return fault.New(fault.Unavailable, "the releases cannot be read")
	}
	if kept != "" {
		rel, err := s.catalog.Release(ctx, kept)
		if err != nil {
			return nil, unavailable(err)
		}
		if rel != nil && !meta.IsStatusConditionFalse(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid) {
			return rel, nil
		}
	}
	name, err := s.catalog.Offered(ctx, channel)
	if err != nil {
		return nil, unavailable(err)
	}
	if name == "" {
		return nil, nil
	}
	rel, err := s.catalog.Release(ctx, name)
	if err != nil {
		return nil, unavailable(err)
	}
	return rel, nil
}

// definitionMessage is a catalog entry as tenants see it. versions is
// what the cluster named in the request is offered; nil without one.
func definitionMessage(e entry, versions map[string]string) (*cloudv1.ComponentDefinition, error) {
	out := &cloudv1.ComponentDefinition{
		Name: e.Name, Description: e.object.Spec.Description, Requires: slices.Clone(e.Requires),
		ConflictsWithAddons: slices.Clone(e.ConflictsWithAddons), Management: management(e.Definition),
		Configurable: configurable(e.Definition), Versions: versions,
	}
	if e.IsPlatform() {
		return out, nil // fixed: its values are not a tenant's concern
	}
	out.UserValues = e.UserValues
	var err error
	if out.ValuesSchema, err = structOf(e.ValuesSchema); err != nil {
		return nil, err
	}
	if out.PlatformValues, err = structOf(e.PlatformValues); err != nil {
		return nil, err
	}
	return out, nil
}

// structOf is values as a Struct; nil for none.
func structOf(v core.Values) (*structpb.Struct, error) {
	if len(v) == 0 {
		return nil, nil
	}
	out, err := structpb.NewStruct(v)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Sprintf("values: %v", err))
	}
	return out, nil
}

// ListComponentDefinitions lists the catalog: every valid definition that
// is not a bundle's member, the Platform ones included.
func (s *Service) ListComponentDefinitions(ctx context.Context, req *cloudv1.ListComponentDefinitionsRequest) (_ *cloudv1.ListComponentDefinitionsResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	entries, err := s.entries(ctx)
	if err != nil {
		return nil, err
	}
	var offered map[string]string
	minor := ""
	if id := req.GetClusterId(); id != "" {
		row, err := s.cluster(ctx, req.GetTenantId(), id)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, noCluster(req.GetTenantId(), id)
		}
		t, err := s.targetOf(ctx, row)
		if err != nil {
			return nil, err
		}
		if t.offering != nil {
			offered, minor = t.offering.Versions, t.cluster.KubernetesMinor
		}
	}
	res := &cloudv1.ListComponentDefinitionsResponse{}
	off := core.Off(definitions(entries))
	for _, e := range entries {
		if !e.Valid || e.Internal {
			continue
		}
		var versions map[string]string
		if v, ok := offered[e.Name]; ok {
			versions = map[string]string{minor: v}
		}
		def, err := definitionMessage(e, versions)
		if err != nil {
			return nil, err
		}
		def.DisabledReason = off[e.Name]
		def.Disabled = def.DisabledReason != ""
		res.Definitions = append(res.Definitions, def)
	}
	return res, nil
}

// GetComponentValues returns a definition's values file as written.
func (s *Service) GetComponentValues(ctx context.Context, req *cloudv1.GetComponentValuesRequest) (_ *cloudv1.GetComponentValuesResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	entries, err := s.entries(ctx)
	if err != nil {
		return nil, err
	}
	e := find(entries, req.GetName())
	if e == nil {
		return nil, fault.Newf(fault.NotFound, "component %s is not in the catalog", req.GetName())
	}
	if e.IsPlatform() {
		return nil, fault.Newf(fault.FailedPrecondition,
			"component %s is installed with the cluster by the platform; it has no values a tenant can set", e.Name)
	}
	if err := core.CheckTenantAccess(e.Definition, definitions(entries)); err != nil {
		return nil, err
	}
	if !configurable(e.Definition) {
		return nil, fault.Newf(fault.FailedPrecondition, "component %s takes no values: enable it without a values file", e.Name)
	}
	return &cloudv1.GetComponentValuesResponse{UserValues: e.UserValues}, nil
}
