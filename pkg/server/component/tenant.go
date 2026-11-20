package component

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	core "github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// noVersion refuses a version a tenant set: it comes from the release.
func noVersion() error {
	return fault.New(fault.Invalid,
		"version cannot be set: a component's version is the one the cluster's release gives it")
}

// accepted is a request that passed the checks: what would be stored, and
// what would be installed.
type accepted struct {
	entry *entry
	// stored are the tenant's values as stored: only what differs from the
	// definition's values file.
	stored core.Values
	// effective are the values the chart would get.
	effective core.Values
	cluster   core.Cluster
	// version and release are what the cluster's release gives the component.
	version, release string
}

// check runs the request checks of an enable or update of name on the
// cluster with values: the values are pruned against the definition's
// values file first (only edits are stored), then checked, then merged.
// enabled are the names of the components the cluster has.
func (s *Service) check(ctx context.Context, tenantID, clusterID, name string, values core.Values, update bool) (*accepted, error) {
	entries, err := s.entries(ctx)
	if err != nil {
		return nil, err
	}
	req := core.Request{Component: name, Catalog: definitions(entries), Update: update}
	out := &accepted{entry: find(entries, name)}
	var file core.Values
	if out.entry != nil {
		if file, err = core.ParseUserValues(out.entry.UserValues); err != nil {
			return nil, fault.Newf(fault.Internal, "component %s: its values file cannot be read", name)
		}
		out.stored = core.PruneUnchanged(values, file)
		req.Values = out.stored
	}
	row, err := s.cluster(ctx, tenantID, clusterID)
	if err != nil {
		return nil, err
	}
	if row != nil {
		t, err := s.targetOf(ctx, row)
		if err != nil {
			return nil, err
		}
		req.Cluster, req.Offering, out.cluster = &t.cluster, t.offering, t.cluster
		rows, err := s.deps.Store.Components().ListByCluster(ctx, tenantID, clusterID)
		if err != nil {
			return nil, service.ToConnect(err)
		}
		for _, c := range rows {
			// One being removed does not count for what requires it; the
			// component itself still exists until it is gone.
			if c.State == phaseDeleting && c.Component != name {
				continue
			}
			req.Enabled = append(req.Enabled, c.Component)
		}
		if out.entry != nil && !out.entry.Covers(t.cluster.Provider) {
			return nil, fault.Newf(fault.FailedPrecondition, "component %s is for %s and cluster %s runs on %s",
				name, out.entry.Provider, row.Name, t.cluster.Provider)
		}
		if out.entry != nil {
			// The release's values for the component are the platform's too:
			// locked, and last in the merge.
			def := core.WithPinValues(out.entry.Definition, t.pins[name])
			req.Definition = &def
			if t.offering != nil {
				out.version, out.release = t.offering.Versions[name], t.offering.Release
			}
		}
	} else if out.entry != nil {
		req.Definition = &out.entry.Definition
	}
	problems := core.CheckEnable(req)
	if err := core.Err(problems); err != nil {
		return nil, err
	}
	if err := core.CheckCluster(name, req.Definition.CloudResources, *req.Cluster); err != nil {
		return nil, err
	}
	out.effective = core.Merge(nil, file, out.stored, req.Definition.PlatformValues)
	return out, nil
}

// valuesOf is a request's values as an object; nil for none.
func valuesOf(c *cloudv1.Component) core.Values {
	if c.GetValues() == nil {
		return nil
	}
	return c.GetValues().AsMap()
}

// valuesJSON are values as the store keeps them; nil for none.
func valuesJSON(v core.Values) (model.JSON, error) {
	if len(v) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fault.Newf(fault.Invalid, "values cannot be stored: %v", err)
	}
	return raw, nil
}

// pendingStatus is the status of a component that was accepted: what it
// will run, once something installs it.
func pendingStatus(a *accepted, generation int64) (*cloudv1.ComponentStatus, error) {
	effective, err := structOf(a.effective)
	if err != nil {
		return nil, err
	}
	return &cloudv1.ComponentStatus{
		Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING, Message: pendingMessage,
		ClusterName: a.cluster.Name, TargetVersion: a.version, Release: a.release, EffectiveValues: effective,
		ObservedGeneration: generation - 1,
	}, nil
}

// preview is the component a validate_only call answers with: as it would
// be stored, with no ID.
func preview(tenantID, clusterID string, a *accepted) (*cloudv1.Component, error) {
	st, err := pendingStatus(a, 1)
	if err != nil {
		return nil, err
	}
	st.Message, st.ObservedGeneration = "valid; nothing was stored", 0
	stored, err := structOf(a.stored)
	if err != nil {
		return nil, err
	}
	return &cloudv1.Component{TenantId: tenantID, ClusterId: clusterID, Component: a.entry.Name, Version: a.version,
		Values: stored, Status: st, Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_TENANT}, nil
}

// view is a stored component as the API returns it.
func view(row *model.Component) (*cloudv1.Component, error) {
	out := &cloudv1.Component{TenantId: row.TenantID, Id: row.ID, ClusterId: row.ClusterID, Component: row.Component,
		Etag: row.ETag, Generation: row.Generation, Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_TENANT,
		Status: &cloudv1.ComponentStatus{}}
	if len(row.Values) > 0 {
		out.Values = &structpb.Struct{}
		if err := protojson.Unmarshal(row.Values, out.Values); err != nil {
			return nil, connect.NewError(connect.CodeInternal, "component "+row.ID+": stored values: "+err.Error())
		}
	}
	if len(row.Status) > 0 {
		if err := reconcilecluster.Lenient.Unmarshal(row.Status, out.Status); err != nil {
			out.Status = &cloudv1.ComponentStatus{}
		}
	}
	if out.Status.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_UNSPECIFIED {
		out.Status.Phase = cloudv1.ComponentPhase(cloudv1.ComponentPhase_value[row.State])
	}
	if row.State == phaseDeleting && out.Status.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING {
		// Disabled: being removed, whatever the last pass reported.
		out.Status.Phase, out.Status.Message = cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING, "being removed from the cluster"
	}
	out.Version = out.Status.GetTargetVersion()
	if v := out.Status.GetInstalledVersion(); v != "" {
		out.Version = v
	}
	if out.Status.GetSince() == nil && !row.UpdatedAt.IsZero() {
		out.Status.Since = timestamppb.New(row.UpdatedAt)
	}
	return out, nil
}

// record writes the accepted request's status for row and returns the
// component as stored.
func (s *Service) record(ctx context.Context, row *model.Component, a *accepted) (*cloudv1.Component, error) {
	st, err := pendingStatus(a, row.Generation)
	if err != nil {
		return nil, err
	}
	raw, err := protojson.Marshal(st)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "component status: "+err.Error())
	}
	// A status that cannot be written is not the caller's problem: the
	// component is stored, and reads fall back to its stored state.
	if err := s.deps.Store.Components().SetStatus(ctx, row.TenantID, row.ID, row.ETag, raw); err != nil {
		s.logger().WarnContext(ctx, "record the component's status", "component_id", row.ID, "error", err)
	} else {
		row.Status = raw
	}
	return view(row)
}

// EnableComponent checks the request and stores the component, PENDING.
// A retry with the same request_id returns the component the first call
// stored. Nothing is installed by this call.
func (s *Service) EnableComponent(ctx context.Context, req *cloudv1.EnableComponentRequest) (_ *cloudv1.EnableComponentResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	in := req.GetComponent()
	if rid := req.GetRequestId(); rid != "" && !req.GetValidateOnly() {
		if row, err := s.deps.Store.Components().GetByCreateRequest(ctx, req.GetTenantId(), rid); err == nil {
			c, err := view(row)
			return &cloudv1.EnableComponentResponse{Component: c}, err
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, service.ToConnect(err)
		}
	}
	if in.GetVersion() != "" {
		return nil, noVersion()
	}
	a, err := s.check(ctx, req.GetTenantId(), in.GetClusterId(), in.GetComponent(), valuesOf(in), false)
	if err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		c, err := preview(req.GetTenantId(), in.GetClusterId(), a)
		return &cloudv1.EnableComponentResponse{Component: c}, err
	}
	values, err := valuesJSON(a.stored)
	if err != nil {
		return nil, err
	}
	row := &model.Component{TenantID: req.GetTenantId(), ClusterID: in.GetClusterId(), Component: in.GetComponent(),
		Values: values, State: phasePending}
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	if err := s.deps.Store.Components().Create(ctx, row); err != nil {
		return nil, createError(err, row, a.cluster.Name)
	}
	s.logger().InfoContext(ctx, "component stored", "tenant_id", row.TenantID, "component_id", row.ID,
		"component", row.Component, "cluster_id", row.ClusterID)
	c, err := s.record(ctx, row, a)
	return &cloudv1.EnableComponentResponse{Component: c}, err
}

// createError maps a failed insert: the cluster went away or is being
// deleted since the checks, or another enable of the component won.
func createError(err error, row *model.Component, cluster string) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return fault.New(fault.NotFound, "the tenant has no such cluster")
	case errors.Is(err, repository.ErrConflict):
		return fault.Newf(fault.FailedPrecondition, "cluster %s is being deleted", cluster)
	case errors.Is(err, repository.ErrAlreadyExists):
		return fault.Newf(fault.AlreadyExists,
			"component %s is already enabled on cluster %s (or the request_id was used before)", row.Component, cluster)
	}
	return service.ToConnect(err)
}

// get reads the tenant's component.
func (s *Service) get(ctx context.Context, tenantID, id string) (*model.Component, error) {
	row, err := s.deps.Store.Components().Get(ctx, tenantID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, "tenant "+tenantID+" has no component "+id)
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return row, nil
}

// stale refuses a request made for another version of the component.
func stale(row *model.Component, etag string) error {
	if etag != "" && etag != row.ETag {
		return fault.Newf(fault.Aborted, "component %s changed since it was read: read it again", row.ID)
	}
	return nil
}

// UpdateComponent replaces the component's values, with the checks of an
// enable. A version is not a tenant's to set.
func (s *Service) UpdateComponent(ctx context.Context, req *cloudv1.UpdateComponentRequest) (_ *cloudv1.UpdateComponentResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	in := req.GetComponent()
	if in.GetVersion() != "" || slices.Contains(req.GetUpdateMask().GetPaths(), "version") {
		return nil, noVersion()
	}
	row, err := s.get(ctx, req.GetTenantId(), in.GetId())
	if err != nil {
		return nil, err
	}
	if err := stale(row, in.GetEtag()); err != nil {
		return nil, err
	}
	if row.State == phaseDeleting {
		return nil, fault.Newf(fault.FailedPrecondition, "component %s is being removed", row.Component)
	}
	a, err := s.check(ctx, row.TenantID, row.ClusterID, row.Component, valuesOf(in), true)
	if err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		c, err := preview(row.TenantID, row.ClusterID, a)
		if c != nil {
			c.Id, c.Etag, c.Generation = row.ID, row.ETag, row.Generation
		}
		return &cloudv1.UpdateComponentResponse{Component: c}, err
	}
	values, err := valuesJSON(a.stored)
	if err != nil {
		return nil, err
	}
	if sameJSON(values, row.Values) {
		c, err := view(row) // nothing changes: no new generation
		return &cloudv1.UpdateComponentResponse{Component: c}, err
	}
	row.Values = values
	row.Generation++
	if err := s.deps.Store.Components().Update(ctx, row); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, fault.Newf(fault.Aborted, "component %s changed since it was read: read it again", row.ID)
		}
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "component updated", "tenant_id", row.TenantID, "component_id", row.ID,
		"component", row.Component, "generation", row.Generation)
	c, err := s.record(ctx, row, a)
	return &cloudv1.UpdateComponentResponse{Component: c}, err
}

// sameJSON reports whether two stored documents say the same.
func sameJSON(a, b model.JSON) bool {
	var x, y any
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ra, _ := json.Marshal(x)
	rb, _ := json.Marshal(y)
	return bytes.Equal(ra, rb)
}

// DisableComponent removes the component from its cluster, unless another
// enabled component requires it. The call only marks it DELETING: the
// cluster reconcile uninstalls its charts, then its role, then removes the
// Component. Disabling a component already being removed returns it as
// it is.
func (s *Service) DisableComponent(ctx context.Context, req *cloudv1.DisableComponentRequest) (_ *cloudv1.DisableComponentResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetComponentId())
	if err != nil {
		return nil, err
	}
	if err := stale(row, req.GetEtag()); err != nil {
		return nil, err
	}
	if row.State == phaseDeleting {
		c, err := view(row)
		return &cloudv1.DisableComponentResponse{Component: c}, err
	}
	entries, err := s.entries(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.deps.Store.Components().ListByCluster(ctx, row.TenantID, row.ClusterID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	var enabled []core.Definition
	for _, c := range rows {
		if c.State == phaseDeleting {
			continue // being removed already: it requires nothing any more
		}
		if e := find(entries, c.Component); e != nil {
			enabled = append(enabled, e.Definition)
		}
	}
	if err := core.CheckDisable(row.Component, enabled); err != nil {
		return nil, err
	}
	row.State = phaseDeleting
	if err := s.deps.Store.Components().Update(ctx, row); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, fault.Newf(fault.Aborted, "component %s changed since it was read: read it again", row.ID)
		}
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "component disabled", "tenant_id", row.TenantID, "component_id", row.ID,
		"component", row.Component, "cluster_id", row.ClusterID)
	c, err := view(row)
	if c != nil {
		c.Status.Phase, c.Status.Message = cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING, "being removed from the cluster"
	}
	return &cloudv1.DisableComponentResponse{Component: c}, err
}

// GetComponent returns one of the tenant's components.
func (s *Service) GetComponent(ctx context.Context, req *cloudv1.GetComponentRequest) (_ *cloudv1.GetComponentResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetComponentId())
	if err != nil {
		return nil, err
	}
	c, err := view(row)
	return &cloudv1.GetComponentResponse{Component: c}, err
}

// ListComponents lists the tenant's components, a page at a time; with a
// cluster it lists everything on that cluster (onCluster).
func (s *Service) ListComponents(ctx context.Context, req *cloudv1.ListComponentsRequest) (_ *cloudv1.ListComponentsResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	if id := req.GetClusterId(); id != "" {
		components, err := s.onCluster(ctx, req.GetTenantId(), id)
		return &cloudv1.ListComponentsResponse{Components: components}, err
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultPageSize
	}
	size = min(size, maxPageSize)
	rows, err := s.deps.Store.Components().List(ctx, req.GetTenantId(), repository.Page{After: req.GetPageToken(), Limit: size + 1})
	if err != nil {
		return nil, service.ToConnect(err)
	}
	res := &cloudv1.ListComponentsResponse{}
	if len(rows) > size {
		rows = rows[:size]
		res.NextPageToken = rows[size-1].ID
	}
	for i := range rows {
		c, err := view(&rows[i])
		if err != nil {
			return nil, err
		}
		res.Components = append(res.Components, c)
	}
	return res, nil
}

// onCluster is everything on a cluster, by name: the components the
// platform installed with it, as its status reports them (no ID,
// read-only), the tenant's components, and the catalog's tenant
// components the cluster does not have (no ID, "not enabled").
func (s *Service) onCluster(ctx context.Context, tenantID, clusterID string) ([]*cloudv1.Component, error) {
	row, err := s.cluster(ctx, tenantID, clusterID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, noCluster(tenantID, clusterID)
	}
	entries, err := s.entries(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.deps.Store.Components().ListByCluster(ctx, tenantID, clusterID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	var out []*cloudv1.Component
	seen := map[string]bool{}
	for _, p := range clusterStatus(row).GetPlatformComponents() {
		seen[p.GetName()] = true
		out = append(out, &cloudv1.Component{TenantId: tenantID, ClusterId: clusterID, Component: p.GetName(),
			Version: p.GetVersion(), Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_PLATFORM,
			Status: &cloudv1.ComponentStatus{Phase: p.GetPhase(), Message: p.GetMessage(), ClusterName: row.Name,
				InstalledVersion: p.GetVersion(), Since: p.GetSince(), Chart: p.GetChart(), HelmState: p.GetHelmState(),
				LastError: p.GetLastError(), Failures: p.GetFailures(), Recoveries: p.GetRecoveries(),
				ChartVersion: p.GetChartVersion(), Revision: p.GetRevision(), Digest: p.GetDigest(), Attempts: p.GetAttempts(),
				CountedFailures: p.GetCountedFailures(), NextRetryAt: p.GetNextRetryAt(), LastErrorClass: p.GetLastErrorClass(),
				LastRecovery: p.GetLastRecovery(), LastRecoveryAt: p.GetLastRecoveryAt(), Unchanged: p.GetUnchanged()}})
	}
	for i := range rows {
		if seen[rows[i].Component] {
			continue // a Platform component's name: cannot be a tenant's
		}
		seen[rows[i].Component] = true
		c, err := view(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	off := core.Off(definitions(entries))
	for _, d := range core.TenantCatalog(definitions(entries)) {
		if seen[d.Name] || !d.Valid || !d.Covers(string(row.Provider)) {
			continue // what the cluster has, or cannot have
		}
		message := notEnabled
		switch why := off[d.Name]; {
		case d.Disabled:
			message = disabledMessage
		case why != "":
			message = "off: " + why
		}
		out = append(out, &cloudv1.Component{TenantId: tenantID, ClusterId: clusterID, Component: d.Name,
			Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_TENANT,
			Status:     &cloudv1.ComponentStatus{Message: message, ClusterName: row.Name}})
	}
	slices.SortStableFunc(out, func(a, b *cloudv1.Component) int { return strings.Compare(a.GetComponent(), b.GetComponent()) })
	return out, nil
}
