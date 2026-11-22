package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

const (
	subTenantCluster = "01a1e418-bddd-77b4-8b4f-c7de9131a16b" // examples/subtenant.yaml
	subTenantID      = "01a1e500-0000-7000-8000-000000000001"
)

// subTenantService keeps one sub-tenant and records what it was asked.
type subTenantService struct {
	cloudv1connect.SubTenantServiceClient
	stored  *cloudv1.SubTenant
	creates []*cloudv1.CreateSubTenantRequest
	updates []*cloudv1.UpdateSubTenantRequest
	deletes []*cloudv1.DeleteSubTenantRequest
}

func (s *subTenantService) CreateSubTenant(_ context.Context, req *cloudv1.CreateSubTenantRequest) (*cloudv1.CreateSubTenantResponse, error) {
	s.creates = append(s.creates, req)
	st := proto.CloneOf(req.GetSubtenant())
	st.Id, st.TenantId, st.ClusterId, st.Etag, st.Generation = subTenantID, req.GetTenantId(), req.GetClusterId(), "e1", 1
	st.Status = &cloudv1.SubTenantStatus{State: cloudv1.SubTenantState_SUB_TENANT_STATE_PENDING}
	s.stored = st
	return &cloudv1.CreateSubTenantResponse{Subtenant: st}, nil
}

func (s *subTenantService) GetSubTenant(_ context.Context, req *cloudv1.GetSubTenantRequest) (*cloudv1.GetSubTenantResponse, error) {
	if s.stored == nil || req.GetSubtenantId() != s.stored.GetId() {
		return nil, connect.NewError(connect.CodeNotFound, "subtenant not found")
	}
	return &cloudv1.GetSubTenantResponse{Subtenant: s.stored}, nil
}

func (s *subTenantService) ListSubTenants(context.Context, *cloudv1.ListSubTenantsRequest) (*cloudv1.ListSubTenantsResponse, error) {
	return &cloudv1.ListSubTenantsResponse{Subtenants: []*cloudv1.SubTenant{s.stored}}, nil
}

func (s *subTenantService) UpdateSubTenant(_ context.Context, req *cloudv1.UpdateSubTenantRequest) (*cloudv1.UpdateSubTenantResponse, error) {
	s.updates = append(s.updates, req)
	return &cloudv1.UpdateSubTenantResponse{Subtenant: s.stored}, nil
}

func (s *subTenantService) DeleteSubTenant(_ context.Context, req *cloudv1.DeleteSubTenantRequest) (*cloudv1.DeleteSubTenantResponse, error) {
	s.deletes = append(s.deletes, req)
	return &cloudv1.DeleteSubTenantResponse{Subtenant: s.stored}, nil
}

// storedSubTenant is the example's sub-tenant as the server keeps it.
func storedSubTenant(t *testing.T) *cloudv1.SubTenant {
	t.Helper()
	m, err := LoadSubTenantManifest(filepath.Join("..", "..", "examples", "subtenant.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.SubTenant()
	if err != nil {
		t.Fatal(err)
	}
	st.Id, st.TenantId, st.ClusterId, st.Etag, st.Generation = subTenantID, "acme", subTenantCluster, "e7", 3
	st.Status = &cloudv1.SubTenantStatus{State: cloudv1.SubTenantState_SUB_TENANT_STATE_READY, ObservedGeneration: 3}
	return st
}

// subTenantManifest writes the example with edit applied to its text.
func subTenantManifest(t *testing.T, edit func(string) string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "subtenant.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "subtenant.yaml")
	if err := os.WriteFile(path, []byte(edit(string(raw))), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func same(s string) string { return s }

func TestSubTenantCreateSendsTheConsolesManifest(t *testing.T) {
	svc := &subTenantService{}
	var out bytes.Buffer
	o := Options{Input: subTenantManifest(t, same)}

	if err := Run(t.Context(), SubTenantCreate, Clients{SubTenant: svc}, o, &out, OutputTable); err != nil {
		t.Fatal(err)
	}
	if len(svc.creates) != 1 {
		t.Fatalf("creates = %d", len(svc.creates))
	}
	req := svc.creates[0]
	if req.GetTenantId() != "acme" || req.GetClusterId() != subTenantCluster || req.GetSubtenant().GetSpec().GetName() != "payments" || req.GetRequestId() == "" {
		t.Errorf("request = %v", req)
	}
	// The same manifest is the same request: a retry returns the same allocation.
	if err := Run(t.Context(), SubTenantCreate, Clients{SubTenant: svc}, o, &bytes.Buffer{}, OutputTable); err != nil {
		t.Fatal(err)
	}
	if svc.creates[1].GetRequestId() != req.GetRequestId() {
		t.Errorf("request IDs %s and %s differ for one manifest", req.GetRequestId(), svc.creates[1].GetRequestId())
	}
	for _, want := range []string{"subtenant/payments (" + subTenantID + ") created", "NAME", "payments", "Protected", "Pending"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSubTenantCreateRefusals(t *testing.T) {
	for name, tt := range map[string]struct {
		edit func(string) string
		o    Options
		want string
	}{
		"an id":               {func(s string) string { return strings.Replace(s, "  # id: <SUBTENANT_ID>", "  id: "+subTenantID, 1) }, Options{}, "assigned by the server"},
		"no cluster":          {func(s string) string { return strings.Replace(s, "  clusterId: "+subTenantCluster, "", 1) }, Options{}, "the cluster is required"},
		"another cluster":     {same, Options{Cluster: "01a1e418-0000-7000-8000-000000000009"}, "differ"},
		"another tenant":      {same, Options{Tenant: "globex"}, "differ"},
		"no name":             {func(s string) string { return strings.Replace(s, "  name: payments", "", 1) }, Options{}, "spec.name is required"},
		"another kind":        {func(s string) string { return strings.Replace(s, "kind: SubTenant", "kind: Cluster", 1) }, Options{}, "kind SubTenant"},
		"an unknown metadata": {func(s string) string { return strings.Replace(s, "metadata:\n", "metadata:\n  name: x\n", 1) }, Options{}, "unknown field"},
	} {
		svc := &subTenantService{}
		tt.o.Input = subTenantManifest(t, tt.edit)
		err := Run(t.Context(), SubTenantCreate, Clients{SubTenant: svc}, tt.o, &bytes.Buffer{}, OutputTable)
		if err == nil || !strings.Contains(err.Error(), tt.want) || len(svc.creates) != 0 {
			t.Errorf("%s: err = %v, creates = %d, want %q and no call", name, err, len(svc.creates), tt.want)
		}
	}
}

func TestSubTenantUpdateSendsOnlyWhatChanged(t *testing.T) {
	svc := &subTenantService{stored: storedSubTenant(t)}
	var progress bytes.Buffer
	path := subTenantManifest(t, func(s string) string {
		s = strings.Replace(s, "  # id: <SUBTENANT_ID>", "  id: "+subTenantID, 1)
		s = strings.Replace(s, "displayName: Payments", "displayName: Payments EU", 1)
		return strings.Replace(s, "namespaceKeys: [app]", "namespaceKeys: [app, jobs]", 1)
	})

	err := Run(t.Context(), SubTenantUpdate, Clients{SubTenant: svc}, Options{Input: path, Progress: &progress}, &bytes.Buffer{}, OutputTable)

	if err != nil {
		t.Fatal(err)
	}
	if len(svc.updates) != 1 {
		t.Fatalf("updates = %d", len(svc.updates))
	}
	req := svc.updates[0]
	if got := req.GetUpdateMask().GetPaths(); !slices.Equal(got, []string{"spec.display_name", "spec.namespace_keys"}) {
		t.Errorf("mask = %v", got)
	}
	if req.GetSubtenant().GetEtag() != "e7" || req.GetSubtenant().GetId() != subTenantID || req.GetClusterId() != subTenantCluster {
		t.Errorf("request = %v", req)
	}
	if !strings.Contains(progress.String(), "spec.display_name: Payments → Payments EU") {
		t.Errorf("progress lacks the change:\n%s", progress.String())
	}
}

func TestSubTenantUpdateRefusals(t *testing.T) {
	withID := func(s string) string { return strings.Replace(s, "  # id: <SUBTENANT_ID>", "  id: "+subTenantID, 1) }
	for name, tt := range map[string]struct {
		edit func(string) string
		want string
	}{
		"a dropped namespace key": {func(s string) string {
			return strings.Replace(withID(s), "namespaceKeys: [app]", "namespaceKeys: [web]", 1)
		}, `drops "app"`},
		"a new name":  {func(s string) string { return strings.Replace(withID(s), "name: payments", "name: billing", 1) }, "spec.name cannot be changed"},
		"no id":       {same, "the id is required"},
		"nothing new": {withID, ""},
	} {
		svc := &subTenantService{stored: storedSubTenant(t)}
		err := Run(t.Context(), SubTenantUpdate, Clients{SubTenant: svc}, Options{Input: subTenantManifest(t, tt.edit)}, &bytes.Buffer{}, OutputTable)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: err = %v, want %q", name, err, tt.want)
		case len(svc.updates) != 0:
			t.Errorf("%s: %d updates sent", name, len(svc.updates))
		}
	}
}

func TestSubTenantDeleteNeedsProtectionOff(t *testing.T) {
	svc := &subTenantService{stored: storedSubTenant(t)}
	o := Options{Tenant: "acme", Cluster: subTenantCluster, ID: subTenantID}

	err := Run(t.Context(), SubTenantDelete, Clients{SubTenant: svc}, o, &bytes.Buffer{}, OutputTable)
	if err == nil || !strings.Contains(err.Error(), "deletion protection") || len(svc.deletes) != 0 {
		t.Fatalf("protected: err = %v, deletes = %d", err, len(svc.deletes))
	}

	off := false
	svc.stored.Spec.DeletionProtection = &off
	var out bytes.Buffer
	if err := Run(t.Context(), SubTenantDelete, Clients{SubTenant: svc}, o, &out, OutputTable); err != nil {
		t.Fatal(err)
	}
	if len(svc.deletes) != 1 || svc.deletes[0].GetEtag() != "e7" {
		t.Fatalf("deletes = %v", svc.deletes)
	}
	if !strings.Contains(out.String(), "subtenant/payments ("+subTenantID+") deletion requested") {
		t.Errorf("output:\n%s", out.String())
	}

	if err := Run(t.Context(), SubTenantDelete, Clients{SubTenant: svc}, Options{Tenant: "acme", ID: subTenantID}, &bytes.Buffer{}, OutputTable); err == nil ||
		!strings.Contains(err.Error(), "--cluster is required") {
		t.Errorf("no cluster: %v", err)
	}
}

func TestSubTenantGetAndListTables(t *testing.T) {
	withClock(t)
	svc := &subTenantService{stored: storedSubTenant(t)}
	svc.stored.Status.Namespaces = []*cloudv1.SubTenantNamespaceStatus{{Key: "app", Name: "payments-app",
		Conditions: []*cloudv1.Condition{{Type: "Ready", Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE}}}}
	svc.stored.Status.DeletionBlockers = []string{"namespace payments-app has 2 pods"}
	var got bytes.Buffer
	o := Options{Tenant: "acme", Cluster: subTenantCluster, ID: subTenantID}
	if err := Run(t.Context(), SubTenantGet, Clients{SubTenant: svc}, o, &got, OutputTable); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Name:", "payments", "Deletion protection:", "on (set spec.deletionProtection: false to delete)",
		"Quota:", "requests.cpu", "Namespaces:", "payments-app", "True", "Deletion blockers:", "has 2 pods"} {
		if !strings.Contains(got.String(), want) {
			t.Errorf("get lacks %q:\n%s", want, got.String())
		}
	}

	var list bytes.Buffer
	if err := Run(t.Context(), SubTenantList, Clients{SubTenant: svc}, Options{Tenant: "acme"}, &list, OutputWide); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(list.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "MESSAGE") ||
		!strings.Contains(lines[1], subTenantCluster) || !strings.Contains(lines[1], "Ready") {
		t.Errorf("list:\n%s", list.String())
	}
}

func TestSubTenantWaitObservesTheCurrentGeneration(t *testing.T) {
	st := &cloudv1.SubTenant{Generation: 4, Status: &cloudv1.SubTenantStatus{
		State: cloudv1.SubTenantState_SUB_TENANT_STATE_READY, ObservedGeneration: 3}}
	if obs := subTenantObservation(st, false); obs.done {
		t.Error("ready at an older generation is not done")
	}
	st.Status.ObservedGeneration = 4
	if obs := subTenantObservation(st, false); !obs.done || obs.failed || obs.state != "Ready" {
		t.Errorf("observation = %+v", obs)
	}
	st.Status.State = cloudv1.SubTenantState_SUB_TENANT_STATE_FAILED
	if obs := subTenantObservation(st, false); !obs.failed {
		t.Errorf("observation = %+v", obs)
	}
}

// The console's example is a valid request as the CLI sends it.
func TestSubTenantExampleIsAValidCreate(t *testing.T) {
	m, err := LoadSubTenantManifest(filepath.Join("..", "..", "examples", "subtenant.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tenant, cluster, err := m.scope(Options{})
	if err != nil || tenant != exampleTenant {
		t.Fatalf("scope = %s, %s, %v", tenant, cluster, err)
	}
	st, err := m.SubTenant()
	req := &cloudv1.CreateSubTenantRequest{TenantId: tenant, ClusterId: cluster, Subtenant: st}
	if err == nil {
		req.RequestId, err = requestID(tenant, req)
	}
	valid(t, "subtenant.yaml", req, err)
}
