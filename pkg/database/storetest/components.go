package storetest

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// componentTests are the behaviour of Components, run by Run.
var componentTests = []struct {
	name string
	test func(*testing.T, Backend)
}{
	{"ComponentRoundTripsAndIsUniquePerCluster", testComponentRoundTripsAndIsUniquePerCluster},
	{"ComponentValuesAreASmallObject", testComponentValuesAreASmallObject},
	{"ComponentNeedsItsLiveCluster", testComponentNeedsItsLiveCluster},
	{"ComponentCreateAndClusterDeleteCannotBothWin", testComponentCreateAndClusterDeleteCannotBothWin},
	{"ClusterWithComponentsCannotBeDeleted", testClusterWithComponentsCannotBeDeleted},
	{"ComponentUpdateChecksETagAndIdentity", testComponentUpdateChecksETagAndIdentity},
	{"ComponentListsAndCreateRequests", testComponentListsAndCreateRequests},
}

// MustComponent enables the component on the cluster.
func MustComponent(t *testing.T, s repository.Store, cluster *model.Cluster, component string) *model.Component {
	t.Helper()
	c := &model.Component{TenantID: cluster.TenantID, ClusterID: cluster.ID, Component: component,
		State: "COMPONENT_PHASE_PENDING"}
	if err := s.Components().Create(t.Context(), c); err != nil {
		t.Fatalf("enable %s: %v", component, err)
	}
	return c
}

func testComponentRoundTripsAndIsUniquePerCluster(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	n := MustNetwork(t, s, "acme", "n")
	prod, staging := MustCluster(t, s, "acme", "prod", n), MustCluster(t, s, "acme", "staging", n)

	c := &model.Component{TenantID: "acme", ClusterID: prod.ID, Component: "karpenter", Version: "1.8.1",
		Values: model.JSON(`{"replicas":2,"settings":{"batchMaxDuration":"10s"}}`), State: "COMPONENT_PHASE_PENDING"}
	if err := s.Components().Create(ctx, c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.ID == "" || c.ETag == "" || c.Generation != 1 || c.CreatedAt.IsZero() {
		t.Fatalf("create assigned id %q, etag %q, generation %d, created %v", c.ID, c.ETag, c.Generation, c.CreatedAt)
	}
	got, err := s.Components().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClusterID != prod.ID || got.Component != "karpenter" || got.Version != "1.8.1" || got.ETag != c.ETag ||
		got.Generation != 1 || got.State != c.State || !sameDocument(got.Values, c.Values) {
		t.Errorf("Get = %+v, want %+v", got, c)
	}

	again := &model.Component{TenantID: "acme", ClusterID: prod.ID, Component: "karpenter"}
	RequireErr(t, s.Components().Create(ctx, again), repository.ErrAlreadyExists)
	MustComponent(t, s, staging, "karpenter")
	MustComponent(t, s, prod, "kyverno")
}

func testComponentValuesAreASmallObject(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	cluster := MustCluster(t, s, "acme", "prod", MustNetwork(t, s, "acme", "n"))
	for name, values := range map[string]string{
		"a list":    `[1,2]`,
		"a string":  `"replicas"`,
		"not JSON":  `{replicas: 2`,
		"too large": `{"blob":"` + strings.Repeat("x", 64<<10) + `"}`,
	} {
		c := &model.Component{TenantID: "acme", ClusterID: cluster.ID, Component: "karpenter", Values: model.JSON(values)}
		if err := s.Components().Create(ctx, c); err == nil {
			t.Errorf("values that are %s: stored", name)
		} else {
			RequireErr(t, err, repository.ErrInvalid)
		}
	}
	rows, err := s.Components().ListByCluster(ctx, "acme", cluster.ID)
	if err != nil || len(rows) != 0 {
		t.Errorf("%d components stored (%v), want none", len(rows), err)
	}
	// No values, and an explicit null, are none.
	MustComponent(t, s, cluster, "karpenter")
	null := &model.Component{TenantID: "acme", ClusterID: cluster.ID, Component: "kyverno", Values: model.JSON("null")}
	if err := s.Components().Create(ctx, null); err != nil {
		t.Errorf("null values: %v", err)
	}
}

func testComponentNeedsItsLiveCluster(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	MustTenant(t, s, "globex")
	n := MustNetwork(t, s, "acme", "n")
	cluster := MustCluster(t, s, "acme", "prod", n)
	theirs := MustCluster(t, s, "globex", "prod", networkInOtherAccount(t, s, "globex", "n"))

	missing := &model.Component{TenantID: "acme", ClusterID: "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33", Component: "karpenter"}
	RequireErr(t, s.Components().Create(ctx, missing), repository.ErrNotFound)
	other := &model.Component{TenantID: "acme", ClusterID: theirs.ID, Component: "karpenter"}
	RequireErr(t, s.Components().Create(ctx, other), repository.ErrNotFound)

	deleting, err := s.Clusters().Get(ctx, "acme", cluster.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleting.State = "CLUSTER_STATE_DELETING"
	if err := s.Clusters().Update(ctx, deleting); err != nil {
		t.Fatal(err)
	}
	late := &model.Component{TenantID: "acme", ClusterID: cluster.ID, Component: "karpenter"}
	RequireErr(t, s.Components().Create(ctx, late), repository.ErrConflict)
}

// A cluster delete marks the cluster DELETING with the etag it read before
// it looked for components: a component stored in between changes that
// etag, so the delete is refused.
func testComponentCreateAndClusterDeleteCannotBothWin(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	cluster := MustCluster(t, s, "acme", "prod", MustNetwork(t, s, "acme", "n"))

	deleting, err := s.Clusters().Get(ctx, "acme", cluster.ID)
	if err != nil {
		t.Fatal(err)
	}
	MustComponent(t, s, cluster, "karpenter")
	deleting.State = "CLUSTER_STATE_DELETING"
	RequireErr(t, s.Clusters().Update(ctx, deleting), repository.ErrConflict)
}

func testClusterWithComponentsCannotBeDeleted(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	cluster := MustCluster(t, s, "acme", "prod", MustNetwork(t, s, "acme", "n"))
	c := MustComponent(t, s, cluster, "karpenter")

	err := s.Clusters().Delete(ctx, "acme", cluster.ID)
	RequireErr(t, err, repository.ErrInUse)
	if !strings.Contains(err.Error(), "karpenter") {
		t.Errorf("error %q does not name the component", err)
	}
	if err := s.Components().Delete(ctx, "acme", c.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	_, err = s.Components().Get(ctx, "acme", c.ID)
	RequireErr(t, err, repository.ErrNotFound)
	if err := s.Clusters().Delete(ctx, "acme", cluster.ID); err != nil {
		t.Fatalf("delete the cluster without components: %v", err)
	}
	RequireErr(t, s.Components().Delete(ctx, "acme", c.ID), repository.ErrNotFound)
}

func testComponentUpdateChecksETagAndIdentity(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	cluster := MustCluster(t, s, "acme", "prod", MustNetwork(t, s, "acme", "n"))
	c := MustComponent(t, s, cluster, "karpenter")
	stale := *c

	c.Values, c.Generation = model.JSON(`{"replicas":3}`), 2
	if err := s.Components().Update(ctx, c); err != nil {
		t.Fatalf("update: %v", err)
	}
	if c.ETag == stale.ETag {
		t.Error("update kept the etag")
	}
	stale.Version = "1.8.2"
	RequireErr(t, s.Components().Update(ctx, &stale), repository.ErrConflict)

	moved := *c
	moved.Component = "kyverno"
	RequireErr(t, s.Components().Update(ctx, &moved), repository.ErrInvalid)
	tooBig := *c
	tooBig.Values = model.JSON(`[]`)
	RequireErr(t, s.Components().Update(ctx, &tooBig), repository.ErrInvalid)

	status, err := protojson.Marshal(&cloudv1.ComponentStatus{Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY,
		InstalledVersion: "1.8.1", ClusterName: "prod", ObservedGeneration: 2})
	if err != nil {
		t.Fatal(err)
	}
	RequireErr(t, s.Components().SetStatus(ctx, "acme", c.ID, stale.ETag, status), repository.ErrConflict)
	if err := s.Components().SetStatus(ctx, "acme", c.ID, c.ETag, status); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, err := s.Components().Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	read := &cloudv1.ComponentStatus{}
	if err := protojson.Unmarshal(got.Status, read); err != nil {
		t.Fatal(err)
	}
	if read.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_READY || read.GetInstalledVersion() != "1.8.1" ||
		read.GetClusterName() != "prod" || read.GetObservedGeneration() != 2 {
		t.Errorf("status = %v", read)
	}
	if got.ETag != c.ETag || got.Generation != 2 || !sameDocument(got.Values, c.Values) {
		t.Errorf("after SetStatus: etag %s generation %d values %s, want %s 2 %s", got.ETag, got.Generation, got.Values, c.ETag, c.Values)
	}
}

func testComponentListsAndCreateRequests(t *testing.T, b Backend) {
	s := b.New(t)
	ctx := t.Context()
	MustTenant(t, s, "acme")
	n := MustNetwork(t, s, "acme", "n")
	prod, staging := MustCluster(t, s, "acme", "prod", n), MustCluster(t, s, "acme", "staging", n)
	request := "0b5e2a8c-1d4f-4e6a-9c3b-7f2d8e1a4b6c"
	first := &model.Component{TenantID: "acme", ClusterID: prod.ID, Component: "kyverno", CreateRequestID: &request}
	if err := s.Components().Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	MustComponent(t, s, prod, "kyverno-policies")
	MustComponent(t, s, staging, "karpenter")

	byRequest, err := s.Components().GetByCreateRequest(ctx, "acme", request)
	if err != nil || byRequest.ID != first.ID {
		t.Fatalf("GetByCreateRequest = %v, %v; want %s", byRequest, err, first.ID)
	}
	retry := &model.Component{TenantID: "acme", ClusterID: staging.ID, Component: "kyverno", CreateRequestID: &request}
	RequireErr(t, s.Components().Create(ctx, retry), repository.ErrAlreadyExists)

	onProd, err := s.Components().ListByCluster(ctx, "acme", prod.ID)
	if err != nil || len(onProd) != 2 || onProd[0].ID != first.ID {
		t.Errorf("ListByCluster(prod) = %d rows (%v), want kyverno then kyverno-policies", len(onProd), err)
	}
	page1, err := s.Components().List(ctx, "acme", repository.Page{Limit: 2})
	if err != nil || len(page1) != 2 {
		t.Fatalf("List page 1 = %d rows (%v)", len(page1), err)
	}
	page2, err := s.Components().List(ctx, "acme", repository.Page{After: page1[1].ID, Limit: 2})
	if err != nil || len(page2) != 1 || page2[0].Component != "karpenter" {
		t.Errorf("List page 2 = %v (%v), want karpenter", page2, err)
	}
	other, err := s.Components().List(ctx, "globex", repository.Page{Limit: 10})
	if err != nil || len(other) != 0 {
		t.Errorf("another tenant lists %d components (%v)", len(other), err)
	}
}
