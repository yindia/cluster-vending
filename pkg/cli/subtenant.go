package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"sigs.k8s.io/yaml"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// KindSubTenant is the manifest kind of a sub-tenant.
const KindSubTenant = "SubTenant"

// subTenantMask are the spec paths an update may change
// (UpdateSubTenantRequest.update_mask). spec.name and the cluster are fixed.
var subTenantMask = []string{
	"spec.access", "spec.deletion_protection", "spec.display_name",
	"spec.namespace_keys", "spec.policy_profile", "spec.quota",
}

// SubTenantManifest is the console's sub-tenant envelope, which the CLI
// reads as it is (examples/subtenant.yaml):
//
//	apiVersion: cluster-vending.io/v1
//	kind: SubTenant
//	metadata:
//	  tenant: acme       # or --tenant
//	  clusterId: <id>    # or --cluster
//	  id: <id>           # update only; assigned on create
//	spec: {name: payments, ...}   # SubTenantSpec, protojson field names
//
// Unlike the other kinds the name is spec.name, not metadata.name.
type SubTenantManifest struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   SubTenantMetadata `json:"metadata"`
	Spec       json.RawMessage   `json:"spec"`
}

// SubTenantMetadata is a sub-tenant manifest's metadata.
type SubTenantMetadata struct {
	Tenant    string `json:"tenant,omitempty"`
	ClusterID string `json:"clusterId,omitempty"`
	ID        string `json:"id,omitempty"`
}

// LoadSubTenantManifest reads a SubTenant manifest from path. Unknown
// fields, at any level, are errors.
func LoadSubTenantManifest(path string) (*SubTenantManifest, error) {
	if path == "" {
		return nil, fmt.Errorf("-f is required: a %s manifest (apiVersion: %s, kind: %s)", KindSubTenant, APIVersion, KindSubTenant)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	js, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("-f %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	m := &SubTenantManifest{}
	if err := dec.Decode(m); err != nil {
		return nil, fmt.Errorf("-f %s: %w", path, err)
	}
	switch {
	case m.APIVersion != APIVersion:
		return nil, fmt.Errorf("-f %s: apiVersion must be %s, got %q", path, APIVersion, m.APIVersion)
	case m.Kind != KindSubTenant:
		return nil, fmt.Errorf("-f %s: this command takes kind %s, got %q", path, KindSubTenant, m.Kind)
	}
	return m, nil
}

// SubTenant is the manifest's spec as a cloud.v1 SubTenant (id from
// metadata.id; tenant and cluster are set by the caller).
func (m *SubTenantManifest) SubTenant() (*cloudv1.SubTenant, error) {
	spec := &cloudv1.SubTenantSpec{}
	if len(m.Spec) > 0 && string(m.Spec) != "null" {
		if err := protojson.Unmarshal(m.Spec, spec); err != nil {
			return nil, fmt.Errorf("SubTenant spec: %w", err)
		}
	}
	return &cloudv1.SubTenant{Id: m.Metadata.ID, Spec: spec}, nil
}

// scope is the manifest's tenant and cluster, or the flags'; both set must
// agree.
func (m *SubTenantManifest) scope(o Options) (tenant, cluster string, err error) {
	tenant = m.Metadata.Tenant
	switch {
	case tenant == "":
		tenant = o.Tenant
	case o.Tenant != "" && !o.tenantDefaulted && o.Tenant != tenant:
		return "", "", fmt.Errorf("metadata.tenant %q and --tenant %q differ", tenant, o.Tenant)
	}
	if tenant == "" {
		return "", "", errors.New("the tenant is required: metadata.tenant or --tenant")
	}
	cluster = m.Metadata.ClusterID
	switch {
	case cluster == "":
		cluster = o.Cluster
	case o.Cluster != "" && o.Cluster != cluster:
		return "", "", fmt.Errorf("metadata.clusterId %q and --cluster %q differ", cluster, o.Cluster)
	}
	if cluster == "" {
		return "", "", errors.New("the cluster is required: metadata.clusterId or --cluster")
	}
	return tenant, cluster, nil
}

// requireSubTenantScope checks --tenant and --cluster of the commands that
// take no manifest.
func (o Options) requireSubTenantScope() error {
	if err := o.requireTenant(); err != nil {
		return err
	}
	if o.Cluster == "" {
		return errors.New("--cluster is required: the ID of the sub-tenant's cluster")
	}
	return nil
}

// SubTenantCreate calls SubTenantService.CreateSubTenant with the
// SubTenant manifest; the server assigns the id. The request ID is derived
// from the request, so running the same manifest again returns the same
// sub-tenant. With --wait it returns once the sub-tenant is READY.
func SubTenantCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadSubTenantManifest(o.Input)
	if err != nil {
		return nil, err
	}
	if m.Metadata.ID != "" {
		return nil, fmt.Errorf("metadata.id %q: a sub-tenant's id is assigned by the server", m.Metadata.ID)
	}
	tenant, cluster, err := m.scope(o)
	if err != nil {
		return nil, err
	}
	st, err := m.SubTenant()
	if err != nil {
		return nil, err
	}
	if st.GetSpec().GetName() == "" {
		return nil, errors.New("spec.name is required: the sub-tenant's name, unique in its cluster")
	}
	req := &cloudv1.CreateSubTenantRequest{TenantId: tenant, ClusterId: cluster, Subtenant: st}
	if req.RequestId, err = requestID(tenant, req); err != nil {
		return nil, err
	}
	res, err := call(ctx, req, c.SubTenant.CreateSubTenant)
	if err != nil || !o.Wait {
		return res, err
	}
	return waitSubTenant(ctx, c, o, tenant, cluster, res.(*cloudv1.CreateSubTenantResponse).GetSubtenant().GetId(), false)
}

// SubTenantGet calls SubTenantService.GetSubTenant.
func SubTenantGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireSubTenantScope(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.GetSubTenantRequest{TenantId: o.Tenant, ClusterId: o.Cluster, SubtenantId: o.ID}, c.SubTenant.GetSubTenant)
}

// SubTenantList calls SubTenantService.ListSubTenants: the tenant's
// sub-tenants, or one cluster's with --cluster.
func SubTenantList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.ListSubTenantsRequest{
		TenantId: o.Tenant, ClusterId: o.Cluster, PageSize: o.PageSize, PageToken: o.PageToken,
	}, c.SubTenant.ListSubTenants)
}

// SubTenantUpdate calls SubTenantService.UpdateSubTenant with the
// SubTenant manifest; the sub-tenant is the argument or metadata.id. The
// mask is what the manifest changes, printed first, sent with the etag
// read. spec.name cannot change and namespace keys are append-only.
func SubTenantUpdate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadSubTenantManifest(o.Input)
	if err != nil {
		return nil, err
	}
	tenant, cluster, err := m.scope(o)
	if err != nil {
		return nil, err
	}
	if m.Metadata.ID, err = updateID(o.ID, m.Metadata.ID); err != nil {
		return nil, err
	}
	desired, err := m.SubTenant()
	if err != nil {
		return nil, err
	}
	got, err := call(ctx, &cloudv1.GetSubTenantRequest{TenantId: tenant, ClusterId: cluster, SubtenantId: desired.GetId()}, c.SubTenant.GetSubTenant)
	if err != nil {
		return nil, err
	}
	current := got.(*cloudv1.GetSubTenantResponse).GetSubtenant()
	if err := subTenantKeysKept(current.GetSpec(), desired.GetSpec()); err != nil {
		return nil, err
	}
	mask, err := inferredMask(o, "subtenant "+current.GetSpec().GetName(),
		&cloudv1.SubTenant{Spec: current.GetSpec()}, &cloudv1.SubTenant{Spec: desired.GetSpec()}, subTenantMask)
	if err != nil || len(mask) == 0 {
		return nil, err
	}
	desired.Etag = current.GetEtag()
	res, err := call(ctx, &cloudv1.UpdateSubTenantRequest{
		TenantId: tenant, ClusterId: cluster, Subtenant: desired, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask},
	}, c.SubTenant.UpdateSubTenant)
	if err != nil || !o.Wait {
		return res, err
	}
	return waitSubTenant(ctx, c, o, tenant, cluster, desired.GetId(), false)
}

// subTenantKeysKept refuses a manifest that drops a namespace key: keys
// are append-only (their namespaces exist).
func subTenantKeysKept(current, desired *cloudv1.SubTenantSpec) error {
	if desired.GetNamespaceKeys() == nil {
		return nil // not stated: unchanged
	}
	for _, k := range current.GetNamespaceKeys() {
		if !slices.Contains(desired.GetNamespaceKeys(), k) {
			return fmt.Errorf("spec.namespaceKeys drops %q: namespace keys can be added, not removed or renamed", k)
		}
	}
	return nil
}

// SubTenantDelete calls SubTenantService.DeleteSubTenant, which stores the
// deletion. Without --etag the sub-tenant is read first for its etag. A
// sub-tenant with deletion protection is refused before the call: update
// spec.deletionProtection to false first. --wait reads until it is gone.
func SubTenantDelete(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireSubTenantScope(); err != nil {
		return nil, err
	}
	etag := o.ETag
	if etag == "" {
		got, err := call(ctx, &cloudv1.GetSubTenantRequest{TenantId: o.Tenant, ClusterId: o.Cluster, SubtenantId: o.ID}, c.SubTenant.GetSubTenant)
		if err != nil {
			return nil, err
		}
		st := got.(*cloudv1.GetSubTenantResponse).GetSubtenant()
		if st.GetSpec().GetDeletionProtection() || st.GetSpec().DeletionProtection == nil {
			return nil, fmt.Errorf("subtenant %s has deletion protection: set spec.deletionProtection: false with subtenant update first", st.GetSpec().GetName())
		}
		etag = st.GetEtag()
	}
	res, err := call(ctx, &cloudv1.DeleteSubTenantRequest{TenantId: o.Tenant, ClusterId: o.Cluster, SubtenantId: o.ID, Etag: etag}, c.SubTenant.DeleteSubTenant)
	if err != nil || !o.Wait {
		return res, err
	}
	if _, err := waitSubTenant(ctx, c, o, o.Tenant, o.Cluster, o.ID, true); err != nil {
		return nil, err
	}
	return res, nil
}

// waitSubTenant waits until the sub-tenant is READY at its current
// generation (gone: deleted) and returns its last read. FAILED ends the
// wait with its message; a call only stores the desired state.
func waitSubTenant(ctx context.Context, c Clients, o Options, tenant, cluster, id string, gone bool) (proto.Message, error) {
	var last proto.Message
	err := waitUntil(ctx, o, "subtenant "+id, gone, func(ctx context.Context) (observation, error) {
		res, err := call(ctx, &cloudv1.GetSubTenantRequest{TenantId: tenant, ClusterId: cluster, SubtenantId: id}, c.SubTenant.GetSubTenant)
		if err != nil {
			return observation{}, err
		}
		last = res
		st := res.(*cloudv1.GetSubTenantResponse).GetSubtenant()
		return subTenantObservation(st, gone), nil
	})
	return last, err
}

// subTenantObservation is one read of a sub-tenant while waiting: done when
// READY and the controller has seen this generation.
func subTenantObservation(st *cloudv1.SubTenant, gone bool) observation {
	state := st.GetStatus().GetState()
	current := st.GetStatus().GetObservedGeneration() >= st.GetGeneration()
	return observation{
		state: word(state.String(), "SUB_TENANT_STATE_"), message: st.GetStatus().GetMessage(),
		done:   !gone && current && state == cloudv1.SubTenantState_SUB_TENANT_STATE_READY,
		failed: !gone && current && state == cloudv1.SubTenantState_SUB_TENANT_STATE_FAILED,
	}
}
