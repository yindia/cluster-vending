package component

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

const tenant = "acme"

// fakeCatalog is a catalog and one release, offered by every channel.
type fakeCatalog struct {
	defs    []cloudv1alpha1.ComponentDefinition
	release *cloudv1alpha1.Release
	err     error
}

func (f *fakeCatalog) Definitions(context.Context) ([]cloudv1alpha1.ComponentDefinition, error) {
	return f.defs, f.err
}

func (f *fakeCatalog) Offered(context.Context, string) (string, error) {
	if f.release == nil {
		return "", nil
	}
	return f.release.Name, nil
}

func (f *fakeCatalog) Release(_ context.Context, name string) (*cloudv1alpha1.Release, error) {
	if f.release == nil || f.release.Name != name {
		return nil, nil
	}
	return f.release, nil
}

func raw(json string) *runtime.RawExtension { return &runtime.RawExtension{Raw: []byte(json)} }

func chart(name string) *cloudv1alpha1.ComponentInstall {
	return &cloudv1alpha1.ComponentInstall{Helm: &cloudv1alpha1.HelmInstall{
		HelmChartSource: cloudv1alpha1.HelmChartSource{Repository: "https://charts.example.com", Chart: name}}}
}

// policiesValues is the values file of the configurable tenant component.
const policiesValues = "# baseline or restricted.\nlevel: baseline\n# Audit or Enforce.\naction: Audit\n"

// catalog is: karpenter and mesh (a bundle of mesh-base) by the platform;
// policies (configurable, requires scanner) and scanner (no values) by
// tenants; and one definition that is not valid.
func catalog() []cloudv1alpha1.ComponentDefinition {
	def := func(name string, spec cloudv1alpha1.ComponentDefinitionSpec) cloudv1alpha1.ComponentDefinition {
		if spec.Install == nil && len(spec.Includes) == 0 {
			spec.Install = chart(name)
		}
		if spec.Namespace == "" && len(spec.Includes) == 0 {
			spec.Namespace = name
		}
		spec.Description = "The " + name + " component."
		return cloudv1alpha1.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	}
	platform := cloudv1alpha1.ComponentManagementPlatform
	return []cloudv1alpha1.ComponentDefinition{
		def("karpenter", cloudv1alpha1.ComponentDefinitionSpec{Management: platform}),
		def("mesh", cloudv1alpha1.ComponentDefinitionSpec{Management: platform, Includes: []string{"mesh-base"}}),
		def("mesh-base", cloudv1alpha1.ComponentDefinitionSpec{Management: platform, Internal: true}),
		def("policies", cloudv1alpha1.ComponentDefinitionSpec{Requires: []string{"scanner"}, ConflictsWithAddons: []string{"cert-manager"},
			PlatformValues: raw(`{"failurePolicy":"Fail"}`), UserValues: policiesValues,
			ValuesSchema: raw(`{"type":"object","additionalProperties":false,"properties":{
				"level":{"type":"string","enum":["baseline","restricted"]},"action":{"type":"string","enum":["Audit","Enforce"]}}}`)}),
		def("scanner", cloudv1alpha1.ComponentDefinitionSpec{}),
		def("broken", cloudv1alpha1.ComponentDefinitionSpec{UserValues: "- not an object\n"}),
	}
}

func release(pins ...string) *cloudv1alpha1.Release {
	k := cloudv1alpha1.KubernetesRelease{Minor: "1.37", Default: true}
	for _, name := range pins {
		k.Components = append(k.Components, cloudv1alpha1.ComponentPin{Name: name, Version: "1.2.3"})
	}
	return &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "2026-11-a"},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: []cloudv1alpha1.KubernetesRelease{k}}}}
}

// world is a tenant with a RUNNING cluster on Kubernetes 1.37, a catalog
// and a release that pins every component.
type world struct {
	svc     *Service
	store   repository.Store
	catalog *fakeCatalog
	cluster *model.Cluster
	network *model.Network
}

const clusterSpec = `{"controlPlane":{"kubernetesVersion":"1.37"},"aws":{"irsa":true,"addons":[{"name":"metrics-server"}]}}`

func newWorld(t *testing.T) world {
	t.Helper()
	store := storetest.NewStore(t)
	storetest.MustTenant(t, store, tenant)
	network := storetest.MustNetwork(t, store, tenant, "net")
	w := world{store: store, network: network,
		catalog: &fakeCatalog{defs: catalog(), release: release("karpenter", "mesh-base", "policies", "scanner")}}
	w.cluster = w.newCluster(t, "prod", reconcilecluster.StateRunning, clusterSpec)
	w.svc = NewService(reconcile.Deps{Store: store}, w.catalog)
	return w
}

func (w world) newCluster(t *testing.T, name, state, spec string) *model.Cluster {
	t.Helper()
	c := &model.Cluster{TenantID: tenant, Name: name, NetworkID: w.network.ID, Provider: "aws", Region: "us-east-1",
		Resource: model.Resource{State: state, Spec: model.JSON(spec)}}
	if err := w.store.Clusters().Create(t.Context(), c); err != nil {
		t.Fatalf("create cluster %s: %v", name, err)
	}
	return c
}

func values(t *testing.T, v map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (w world) enable(t *testing.T, name string, v map[string]any) *cloudv1.Component {
	t.Helper()
	req := &cloudv1.EnableComponentRequest{TenantId: tenant, Component: &cloudv1.Component{ClusterId: w.cluster.ID, Component: name}}
	if v != nil {
		req.Component.Values = values(t, v)
	}
	res, err := w.svc.EnableComponent(t.Context(), req)
	if err != nil {
		t.Fatalf("enable %s: %v", name, err)
	}
	return res.GetComponent()
}

// requireCode fails unless err is a Connect error of code whose message
// has every want.
func requireCode(t *testing.T, err error, code connect.Code, want ...string) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
	for _, w := range want {
		if !strings.Contains(ce.Message(), w) {
			t.Errorf("message %q does not name %q", ce.Message(), w)
		}
	}
}

func TestCatalogListsEveryComponentWithWhoManagesIt(t *testing.T) {
	w := newWorld(t)

	res, err := w.svc.ListComponentDefinitions(t.Context(), &cloudv1.ListComponentDefinitionsRequest{TenantId: tenant, ClusterId: w.cluster.ID})

	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range res.GetDefinitions() {
		line := d.GetName() + " " + strings.TrimPrefix(d.GetManagement().String(), "COMPONENT_MANAGEMENT_")
		if d.GetConfigurable() {
			line += " configurable"
		}
		if v := d.GetVersions()["1.37"]; v != "" {
			line += " " + v
		}
		got = append(got, line)
	}
	// mesh-base is a bundle's member and broken is not valid: neither is
	// listed. The bundle has no chart, so no version, of its own.
	want := []string{"karpenter PLATFORM 1.2.3", "mesh PLATFORM", "policies TENANT configurable 1.2.3", "scanner TENANT 1.2.3"}
	if !slices.Equal(got, want) {
		t.Fatalf("catalog = %v, want %v", got, want)
	}
	for _, d := range res.GetDefinitions() {
		platform := d.GetManagement() == cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_PLATFORM
		if platform && (d.GetUserValues() != "" || d.GetPlatformValues() != nil) {
			t.Errorf("%s shows values to tenants", d.GetName())
		}
		if d.GetName() == "policies" && (d.GetUserValues() != policiesValues || d.GetValuesSchema() == nil ||
			!slices.Equal(d.GetRequires(), []string{"scanner"}) || d.GetDescription() == "") {
			t.Errorf("policies = %v", d)
		}
	}
}

func TestCatalogFailures(t *testing.T) {
	tests := []struct {
		name string
		edit func(*world)
		req  *cloudv1.ListComponentDefinitionsRequest
		code connect.Code
	}{
		{"a bad tenant", nil, &cloudv1.ListComponentDefinitionsRequest{TenantId: "Not A Tenant"}, connect.CodeInvalidArgument},
		{"a cluster of another tenant", nil,
			&cloudv1.ListComponentDefinitionsRequest{TenantId: tenant, ClusterId: "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"}, connect.CodeNotFound},
		{"the catalog cannot be read", func(w *world) { w.catalog.err = errors.New("boom") },
			&cloudv1.ListComponentDefinitionsRequest{TenantId: tenant}, connect.CodeUnavailable},
		{"no catalog", func(w *world) { w.svc = NewService(reconcile.Deps{Store: w.store}, nil) },
			&cloudv1.ListComponentDefinitionsRequest{TenantId: tenant}, connect.CodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			if tt.edit != nil {
				tt.edit(&w)
			}

			_, err := w.svc.ListComponentDefinitions(t.Context(), tt.req)

			requireCode(t, err, tt.code)
		})
	}
}

func TestGetComponentValues(t *testing.T) {
	tests := []struct {
		name, component string
		code            connect.Code
		want            string
	}{
		{"a configurable component", "policies", 0, policiesValues},
		{"a platform component", "karpenter", connect.CodeFailedPrecondition, "installed with the cluster by the platform"},
		{"a bundle's member", "mesh-base", connect.CodeFailedPrecondition, "installed with the cluster by the platform"},
		{"a component without values", "scanner", connect.CodeFailedPrecondition, "takes no values"},
		{"not in the catalog", "nope", connect.CodeNotFound, "not in the catalog"},
		{"not valid", "broken", connect.CodeNotFound, "not in the catalog"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)

			res, err := w.svc.GetComponentValues(t.Context(), &cloudv1.GetComponentValuesRequest{Name: tt.component})

			if tt.code == 0 {
				if err != nil || res.GetUserValues() != tt.want {
					t.Fatalf("values = %q, %v", res.GetUserValues(), err)
				}
				return
			}
			requireCode(t, err, tt.code, tt.want)
		})
	}
}

// Only the tenant's edits are stored, and the component is PENDING with
// what it will run.
func TestEnableStoresOnlyTheEdits(t *testing.T) {
	w := newWorld(t)
	w.enable(t, "scanner", nil)

	c := w.enable(t, "policies", map[string]any{"level": "baseline", "action": "Enforce"})

	if c.GetId() == "" || c.GetEtag() == "" || c.GetGeneration() != 1 || c.GetVersion() != "1.2.3" ||
		c.GetManagement() != cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_TENANT {
		t.Fatalf("component = %v", c)
	}
	if got := c.GetValues().AsMap(); len(got) != 1 || got["action"] != "Enforce" {
		t.Errorf("stored values = %v, want only the edit", got)
	}
	st := c.GetStatus()
	effective := st.GetEffectiveValues().AsMap()
	if st.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING || !strings.Contains(st.GetMessage(), "not installed yet") ||
		st.GetRelease() != "2026-11-a" || st.GetTargetVersion() != "1.2.3" || st.GetClusterName() != "prod" ||
		effective["level"] != "baseline" || effective["action"] != "Enforce" || effective["failurePolicy"] != "Fail" {
		t.Errorf("status = %v", st)
	}
	got, err := w.svc.GetComponent(t.Context(), &cloudv1.GetComponentRequest{TenantId: tenant, ComponentId: c.GetId()})
	if err != nil || got.GetComponent().GetStatus().GetRelease() != "2026-11-a" || got.GetComponent().GetValues().AsMap()["action"] != "Enforce" {
		t.Errorf("GetComponent = %v, %v", got, err)
	}
}

func TestEnableValidateOnlyStoresNothing(t *testing.T) {
	w := newWorld(t)
	w.enable(t, "scanner", nil)
	req := &cloudv1.EnableComponentRequest{TenantId: tenant, ValidateOnly: true,
		Component: &cloudv1.Component{ClusterId: w.cluster.ID, Component: "policies",
			Values: values(t, map[string]any{"level": "restricted", "action": "Audit"})}}

	res, err := w.svc.EnableComponent(t.Context(), req)

	if err != nil {
		t.Fatal(err)
	}
	c := res.GetComponent()
	if c.GetId() != "" || len(c.GetValues().AsMap()) != 1 || c.GetValues().AsMap()["level"] != "restricted" ||
		c.GetStatus().GetEffectiveValues().AsMap()["action"] != "Audit" {
		t.Errorf("preview = %v", c)
	}
	rows, err := w.store.Components().ListByCluster(t.Context(), tenant, w.cluster.ID)
	if err != nil || len(rows) != 1 {
		t.Errorf("stored %d components, %v; want the scanner only", len(rows), err)
	}
}

func TestEnableIsRefused(t *testing.T) {
	other := "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"
	tests := []struct {
		name      string
		edit      func(*testing.T, *world)
		component string
		cluster   string // empty: the world's
		version   string
		values    map[string]any
		code      connect.Code
		want      []string
	}{
		{name: "a platform component", component: "karpenter", code: connect.CodePermissionDenied,
			want: []string{"karpenter is installed with the cluster by the platform"}},
		{name: "a bundle's member", component: "mesh-base", code: connect.CodePermissionDenied, want: []string{"mesh-base"}},
		{name: "not in the catalog", component: "nope", code: connect.CodeNotFound, want: []string{"not in the catalog"}},
		{name: "a definition that is not valid", component: "broken", code: connect.CodeNotFound},
		{name: "a cluster of another tenant", component: "scanner", cluster: other, code: connect.CodeNotFound, want: []string{"no such cluster"}},
		{name: "a version", component: "scanner", version: "9.9.9", code: connect.CodeInvalidArgument, want: []string{"version cannot be set"}},
		{name: "a required component is missing", component: "policies", code: connect.CodeFailedPrecondition, want: []string{"enable scanner first"}},
		{name: "a locked path and an unknown key", component: "policies",
			edit:   func(t *testing.T, w *world) { w.enable(t, "scanner", nil) },
			values: map[string]any{"failurePolicy": "Ignore", "replicas": 3},
			code:   connect.CodeInvalidArgument, want: []string{"only the platform sets failurePolicy", "replicas"}},
		{name: "a value the schema refuses", component: "policies",
			edit:   func(t *testing.T, w *world) { w.enable(t, "scanner", nil) },
			values: map[string]any{"level": "none"}, code: connect.CodeInvalidArgument, want: []string{"level"}},
		{name: "values for a component that takes none", component: "scanner", values: map[string]any{"a": 1},
			code: connect.CodeInvalidArgument, want: []string{"accepts no values"}},
		{name: "already enabled", component: "scanner",
			edit: func(t *testing.T, w *world) { w.enable(t, "scanner", nil) },
			code: connect.CodeAlreadyExists, want: []string{"already enabled on cluster prod"}},
		{name: "a conflicting add-on", component: "policies",
			edit: func(t *testing.T, w *world) {
				w.enable(t, "scanner", nil)
				w.cluster = w.newCluster(t, "with-addon", reconcilecluster.StateRunning,
					`{"controlPlane":{"kubernetesVersion":"1.37"},"aws":{"addons":[{"name":"cert-manager"}]}}`)
			},
			code: connect.CodeFailedPrecondition, want: []string{"remove the cert-manager add-on first"}},
		{name: "a cluster that is not running", component: "scanner",
			edit: func(t *testing.T, w *world) {
				w.cluster = w.newCluster(t, "new", reconcilecluster.StateProvisioning, clusterSpec)
			},
			code: connect.CodeFailedPrecondition, want: []string{"PROVISIONING", "once it is RUNNING"}},
		{name: "a cluster being deleted", component: "scanner",
			edit: func(t *testing.T, w *world) {
				w.cluster = w.newCluster(t, "going", reconcilecluster.StateDeleting, clusterSpec)
			},
			code: connect.CodeFailedPrecondition, want: []string{"is being deleted"}},
		{name: "not in the cluster's release", component: "scanner",
			edit: func(_ *testing.T, w *world) { w.catalog.release = release("policies") },
			code: connect.CodeFailedPrecondition, want: []string{"scanner is not offered on Kubernetes 1.37 in release 2026-11-a"}},
		{name: "no release", component: "scanner",
			edit: func(_ *testing.T, w *world) { w.catalog.release = nil },
			code: connect.CodeFailedPrecondition, want: []string{"no release applies"}},
		{name: "a Kubernetes minor the release does not have", component: "scanner",
			edit: func(t *testing.T, w *world) {
				w.cluster = w.newCluster(t, "old", reconcilecluster.StateRunning, `{"controlPlane":{"kubernetesVersion":"1.30"}}`)
			},
			code: connect.CodeFailedPrecondition, want: []string{"Kubernetes 1.30 is outside the support window"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			if tt.edit != nil {
				tt.edit(t, &w)
			}
			cluster := tt.cluster
			if cluster == "" {
				cluster = w.cluster.ID
			}
			req := &cloudv1.EnableComponentRequest{TenantId: tenant,
				Component: &cloudv1.Component{ClusterId: cluster, Component: tt.component, Version: tt.version}}
			if tt.values != nil {
				req.Component.Values = values(t, tt.values)
			}

			_, err := w.svc.EnableComponent(t.Context(), req)

			requireCode(t, err, tt.code, tt.want...)
		})
	}
}

// A retry with the same request_id returns the component of the first call.
func TestEnableIsIdempotentByRequestID(t *testing.T) {
	w := newWorld(t)
	req := &cloudv1.EnableComponentRequest{TenantId: tenant, RequestId: "5f2c8a8e-6b1d-4c0e-9a1b-7c3d2e1f0a9b",
		Component: &cloudv1.Component{ClusterId: w.cluster.ID, Component: "scanner"}}
	first, err := w.svc.EnableComponent(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}

	again, err := w.svc.EnableComponent(t.Context(), req)

	if err != nil || again.GetComponent().GetId() != first.GetComponent().GetId() {
		t.Fatalf("replay = %v, %v; want component %s", again, err, first.GetComponent().GetId())
	}
}

func TestUpdateReplacesTheValues(t *testing.T) {
	w := newWorld(t)
	w.enable(t, "scanner", nil)
	c := w.enable(t, "policies", map[string]any{"action": "Enforce"})
	mask := &fieldmaskpb.FieldMask{Paths: []string{"values"}}
	update := func(c *cloudv1.Component, validateOnly bool) (*cloudv1.Component, error) {
		res, err := w.svc.UpdateComponent(t.Context(), &cloudv1.UpdateComponentRequest{TenantId: tenant, Component: c,
			UpdateMask: mask, ValidateOnly: validateOnly})
		return res.GetComponent(), err
	}

	// validate_only: answered, nothing stored.
	preview, err := update(&cloudv1.Component{Id: c.GetId(), Values: values(t, map[string]any{"level": "restricted"})}, true)
	if err != nil || preview.GetGeneration() != 1 || preview.GetValues().AsMap()["level"] != "restricted" {
		t.Fatalf("preview = %v, %v", preview, err)
	}
	// The values are replaced as a whole: "action" goes back to the file's.
	got, err := update(&cloudv1.Component{Id: c.GetId(), Etag: c.GetEtag(), Values: values(t, map[string]any{"level": "restricted"})}, false)
	if err != nil {
		t.Fatal(err)
	}
	effective := got.GetStatus().GetEffectiveValues().AsMap()
	if got.GetGeneration() != 2 || got.GetEtag() == c.GetEtag() || len(got.GetValues().AsMap()) != 1 ||
		effective["level"] != "restricted" || effective["action"] != "Audit" {
		t.Errorf("updated = %v", got)
	}
	// The same values again: no new generation.
	same, err := update(&cloudv1.Component{Id: c.GetId(), Values: values(t, map[string]any{"level": "restricted", "action": "Audit"})}, false)
	if err != nil || same.GetGeneration() != 2 {
		t.Errorf("unchanged update = %v, %v", same, err)
	}

	tests := []struct {
		name string
		req  *cloudv1.UpdateComponentRequest
		code connect.Code
		want string
	}{
		{"a stale etag", &cloudv1.UpdateComponentRequest{TenantId: tenant, UpdateMask: mask,
			Component: &cloudv1.Component{Id: c.GetId(), Etag: c.GetEtag(), Values: values(t, map[string]any{"action": "Enforce"})}},
			connect.CodeAborted, "changed since it was read"},
		{"a locked path", &cloudv1.UpdateComponentRequest{TenantId: tenant, UpdateMask: mask,
			Component: &cloudv1.Component{Id: c.GetId(), Values: values(t, map[string]any{"failurePolicy": "Ignore"})}},
			connect.CodeInvalidArgument, "only the platform sets failurePolicy"},
		{"a version", &cloudv1.UpdateComponentRequest{TenantId: tenant, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version"}},
			Component: &cloudv1.Component{Id: c.GetId(), Version: "9.9.9"}}, connect.CodeInvalidArgument, "version cannot be set"},
		{"another mask path", &cloudv1.UpdateComponentRequest{TenantId: tenant, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cluster_id"}},
			Component: &cloudv1.Component{Id: c.GetId()}}, connect.CodeInvalidArgument, "update_mask"},
		{"a component of another tenant", &cloudv1.UpdateComponentRequest{TenantId: "other", UpdateMask: mask,
			Component: &cloudv1.Component{Id: c.GetId()}}, connect.CodeNotFound, "has no component"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := w.svc.UpdateComponent(t.Context(), tt.req)

			requireCode(t, err, tt.code, tt.want)
		})
	}
}

func TestDisable(t *testing.T) {
	w := newWorld(t)
	scanner := w.enable(t, "scanner", nil)
	policies := w.enable(t, "policies", nil)
	disable := func(id, etag string) (*cloudv1.Component, error) {
		res, err := w.svc.DisableComponent(t.Context(), &cloudv1.DisableComponentRequest{TenantId: tenant, ComponentId: id, Etag: etag})
		return res.GetComponent(), err
	}

	_, err := disable(scanner.GetId(), "")
	requireCode(t, err, connect.CodeFailedPrecondition, "scanner is required by policies; disable policies first")
	_, err = disable(policies.GetId(), "not-the-etag")
	requireCode(t, err, connect.CodeAborted)

	gone, err := disable(policies.GetId(), policies.GetEtag())
	if err != nil || gone.GetStatus().GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING {
		t.Fatalf("disable = %v, %v", gone, err)
	}
	_, err = w.svc.GetComponent(t.Context(), &cloudv1.GetComponentRequest{TenantId: tenant, ComponentId: policies.GetId()})
	requireCode(t, err, connect.CodeNotFound)
	// With policies gone the scanner can go, and be enabled again.
	if _, err := disable(scanner.GetId(), ""); err != nil {
		t.Fatal(err)
	}
	w.enable(t, "scanner", nil)
}

// With a cluster the list is everything on it: what the platform
// installed, what the tenant enabled, and what the tenant could enable.
func TestListOnACluster(t *testing.T) {
	w := newWorld(t)
	status := &cloudv1.ClusterStatus{PlatformRelease: "2026-11-a", PlatformComponents: []*cloudv1.PlatformComponent{
		{Name: "karpenter", Version: "1.2.3", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY},
		{Name: "mesh-base", Version: "1.2.3", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Message: "chart mesh-base: boom"},
	}}
	encoded, err := reconcilecluster.Encode(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Clusters().SetStatus(t.Context(), tenant, w.cluster.ID, w.cluster.ETag, encoded); err != nil {
		t.Fatal(err)
	}
	scanner := w.enable(t, "scanner", nil)

	res, err := w.svc.ListComponents(t.Context(), &cloudv1.ListComponentsRequest{TenantId: tenant, ClusterId: w.cluster.ID})

	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range res.GetComponents() {
		got = append(got, strings.Join([]string{c.GetComponent(), strings.TrimPrefix(c.GetManagement().String(), "COMPONENT_MANAGEMENT_"),
			strings.TrimPrefix(c.GetStatus().GetPhase().String(), "COMPONENT_PHASE_"), c.GetVersion(), c.GetStatus().GetMessage()}, "|"))
		if (c.GetId() != "") != (c.GetComponent() == "scanner") {
			t.Errorf("%s has id %q", c.GetComponent(), c.GetId())
		}
	}
	want := []string{
		"karpenter|PLATFORM|READY|1.2.3|",
		"mesh-base|PLATFORM|FAILED|1.2.3|chart mesh-base: boom",
		"policies|TENANT|UNSPECIFIED||not enabled",
		"scanner|TENANT|PENDING|1.2.3|" + pendingMessage,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("list =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	_ = scanner

	_, err = w.svc.ListComponents(t.Context(), &cloudv1.ListComponentsRequest{TenantId: tenant, ClusterId: "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"})
	requireCode(t, err, connect.CodeNotFound)
}

// Without a cluster the list is the tenant's components, a page at a time.
func TestListOfATenantIsPaged(t *testing.T) {
	w := newWorld(t)
	first := w.enable(t, "scanner", nil)
	w.cluster = w.newCluster(t, "staging", reconcilecluster.StateRunning, clusterSpec)
	second := w.enable(t, "scanner", nil)

	page, err := w.svc.ListComponents(t.Context(), &cloudv1.ListComponentsRequest{TenantId: tenant, PageSize: 1})
	if err != nil || len(page.GetComponents()) != 1 || page.GetNextPageToken() == "" {
		t.Fatalf("first page = %v, %v", page, err)
	}
	rest, err := w.svc.ListComponents(t.Context(), &cloudv1.ListComponentsRequest{TenantId: tenant, PageSize: 1, PageToken: page.GetNextPageToken()})

	if err != nil || len(rest.GetComponents()) != 1 || rest.GetNextPageToken() != "" {
		t.Fatalf("second page = %v, %v", rest, err)
	}
	ids := []string{page.GetComponents()[0].GetId(), rest.GetComponents()[0].GetId()}
	slices.Sort(ids)
	want := []string{first.GetId(), second.GetId()}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("listed %v, want %v", ids, want)
	}
}

// The release's values for a component are the platform's: locked.
func TestReleasePinValuesAreLocked(t *testing.T) {
	w := newWorld(t)
	w.enable(t, "scanner", nil)
	rel := release("scanner")
	rel.Spec.Content.Kubernetes[0].Components = append(rel.Spec.Content.Kubernetes[0].Components,
		cloudv1alpha1.ComponentPin{Name: "policies", Version: "2.0.0", Values: raw(`{"action":"Audit"}`)})
	w.catalog.release = rel

	_, err := w.svc.EnableComponent(t.Context(), &cloudv1.EnableComponentRequest{TenantId: tenant,
		Component: &cloudv1.Component{ClusterId: w.cluster.ID, Component: "policies", Values: values(t, map[string]any{"action": "Enforce"})}})

	requireCode(t, err, connect.CodeInvalidArgument, "only the platform sets action")
}

// A definition the platform switched off is listed as disabled, with what
// is off because of it; it cannot be enabled, and a cluster's list says so.
func TestDisabledDefinitions(t *testing.T) {
	w := newWorld(t)
	for i := range w.catalog.defs {
		if w.catalog.defs[i].Name == "scanner" {
			w.catalog.defs[i].Spec.Disabled = true
		}
	}

	res, err := w.svc.ListComponentDefinitions(t.Context(), &cloudv1.ListComponentDefinitionsRequest{TenantId: tenant})

	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range res.GetDefinitions() {
		if d.GetDisabled() != (d.GetDisabledReason() != "") {
			t.Errorf("%s: disabled %v with reason %q", d.GetName(), d.GetDisabled(), d.GetDisabledReason())
		}
		got[d.GetName()] = d.GetDisabledReason()
	}
	want := map[string]string{"karpenter": "", "mesh": "", "scanner": "disabled", "policies": "it requires scanner, which is off"}
	if !maps.Equal(got, want) {
		t.Fatalf("catalog = %v, want %v", got, want)
	}

	for _, name := range []string{"scanner", "policies"} {
		_, err := w.svc.EnableComponent(t.Context(), &cloudv1.EnableComponentRequest{TenantId: tenant,
			Component: &cloudv1.Component{ClusterId: w.cluster.ID, Component: name}})
		requireCode(t, err, connect.CodeFailedPrecondition, name)
	}

	list, err := w.svc.ListComponents(t.Context(), &cloudv1.ListComponentsRequest{TenantId: tenant, ClusterId: w.cluster.ID})
	if err != nil {
		t.Fatal(err)
	}
	messages := map[string]string{}
	for _, c := range list.GetComponents() {
		messages[c.GetComponent()] = c.GetStatus().GetMessage()
	}
	if messages["scanner"] != "disabled by the platform" || messages["policies"] != "off: it requires scanner, which is off" {
		t.Errorf("list messages = %v", messages)
	}
}
