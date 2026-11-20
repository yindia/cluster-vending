package cloudstore_test

import (
	"errors"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudapi "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

var resultsAt = time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC)

func deprecationRef() model.ResultRef {
	return model.ResultRef{Kind: "deprecations", Bucket: "cv-prod-1a2b3c4d-0123456789ab", Key: "latest/deprecations.json",
		ETag: `"abc"`, CollectedAt: resultsAt, AttemptedAt: resultsAt.Add(time.Minute), Total: 12, Removed: 3, Truncated: true}
}

// A summary is stored on the Cluster's status, read back on the row, and
// kept by the reconciler's own status writes.
func TestSetResultRef(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	clusters := w.s.Clusters()

	row, err := clusters.Get(ctx, "acme", c.ID)
	if err != nil || len(row.Results) != 0 || row.ResultRef("deprecations") != nil {
		t.Fatalf("before any: %+v, %v", row.Results, err)
	}
	want := deprecationRef()
	if err := clusters.SetResultRef(ctx, "acme", c.ID, want); err != nil {
		t.Fatal(err)
	}
	row, err = clusters.Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := clusters.SetStatus(ctx, "acme", c.ID, row.ETag, model.JSON(`{"state":"CLUSTER_STATE_RUNNING"}`)); err != nil {
		t.Fatal(err)
	}
	row, err = clusters.Get(ctx, "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := row.ResultRef("deprecations")
	if got == nil || *got != want {
		t.Fatalf("ref = %+v, want %+v", got, want)
	}
	// The listing carries it too: gates read the counts from there.
	listed, err := clusters.List(ctx, "acme", repository.Page{Limit: 10})
	if err != nil || len(listed) == 0 || listed[0].ResultRef("deprecations") == nil {
		t.Errorf("list = %+v, %v", listed, err)
	}
}

// One kind at a time: a write of one kind leaves the others, replaces its
// own, and writes nothing when nothing changed.
func TestSetResultRefReplacesItsKindOnly(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	clusters := w.s.Clusters()
	violations := model.ResultRef{Kind: "violations", Bucket: "cv-prod-1a2b3c4d-0123456789ab", Key: "latest/violations.json", ETag: `"v1"`, Total: 4}
	for _, ref := range []model.ResultRef{deprecationRef(), violations} {
		if err := clusters.SetResultRef(ctx, "acme", c.ID, ref); err != nil {
			t.Fatal(err)
		}
	}
	newer := deprecationRef()
	newer.ETag, newer.Total, newer.Removed = `"def"`, 20, 0
	if err := clusters.SetResultRef(ctx, "acme", c.ID, newer); err != nil {
		t.Fatal(err)
	}
	row, _ := clusters.Get(ctx, "acme", c.ID)
	if len(row.Results) != 2 || *row.ResultRef("deprecations") != newer || *row.ResultRef("violations") != violations {
		t.Fatalf("results = %+v", row.Results)
	}

	var before, after cloudapi.Cluster
	key := client.ObjectKey{Namespace: namespace, Name: "prod"}
	if err := w.k.Get(ctx, key, &before); err != nil {
		t.Fatal(err)
	}
	if err := clusters.SetResultRef(ctx, "acme", c.ID, newer); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Get(ctx, key, &after); err != nil || before.ResourceVersion != after.ResourceVersion {
		t.Errorf("an unchanged ref was written: %s -> %s, %v", before.ResourceVersion, after.ResourceVersion, err)
	}
}

func TestSetResultRefRefusesWhatIsNotAKind(t *testing.T) {
	w := newWorld(t)
	c := w.cluster(t, "prod")
	for name, ref := range map[string]model.ResultRef{
		"unknown kind": {Kind: "secrets", Bucket: "cv-x-0123456789ab", Key: "latest/secrets.json"},
		"no key":       {Kind: "violations", Bucket: "cv-x-0123456789ab"},
		"no bucket":    {Kind: "violations", Key: "latest/violations.json"},
		"foreign key":  {Kind: "violations", Bucket: "cv-x-0123456789ab", Key: "../latest/violations.json"},
		"other bucket": {Kind: "violations", Bucket: "somebody-elses", Key: "latest/violations.json"},
	} {
		if err := w.s.Clusters().SetResultRef(t.Context(), "acme", c.ID, ref); !errors.Is(err, repository.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if err := w.s.Clusters().SetResultRef(t.Context(), "other", c.ID, deprecationRef()); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("another tenant's write: %v", err)
	}
}

// Once a summary names the bucket's object, the full list leaves the
// Cluster: the findings, and the policy snapshot when both of its kinds are
// in the bucket.
func TestSetResultRefDropsWhatTheBucketHolds(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	c := w.cluster(t, "prod")
	clusters := w.s.Clusters()
	if err := clusters.SetDeprecations(ctx, "acme", c.ID, model.JSON(`{"targetVersion":"1.37","total":1,"removed":1,"findings":[{"kind":"Ingress","name":"a"}]}`)); err != nil {
		t.Fatal(err)
	}
	var obj cloudapi.Cluster
	key := client.ObjectKey{Namespace: namespace, Name: "prod"}
	if err := w.k.Get(ctx, key, &obj); err != nil {
		t.Fatal(err)
	}
	obj.Status.PolicySnapshot = `{"summary":{},"violations":[{"id":"a"}],"events":[{"id":"e"}]}`
	if err := w.k.Status().Update(ctx, &obj); err != nil {
		t.Fatal(err)
	}

	if err := clusters.SetResultRef(ctx, "acme", c.ID, deprecationRef()); err != nil {
		t.Fatal(err)
	}
	if err := clusters.SetResultRef(ctx, "acme", c.ID, model.ResultRef{Kind: "violations", Bucket: "cv-x-0123456789ab", Key: "latest/violations.json"}); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Get(ctx, key, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.Deprecations == nil || len(obj.Status.Deprecations.Findings) != 0 || obj.Status.Deprecations.Total != 1 {
		t.Errorf("deprecations = %+v, want the counts without the findings", obj.Status.Deprecations)
	}
	if obj.Status.PolicySnapshot == "" {
		t.Error("the policy snapshot was dropped while its events are not in the bucket")
	}
	if err := clusters.SetResultRef(ctx, "acme", c.ID, model.ResultRef{Kind: "violation-events", Bucket: "cv-x-0123456789ab", Key: "latest/violation-events.json"}); err != nil {
		t.Fatal(err)
	}
	if err := w.k.Get(ctx, key, &obj); err != nil || obj.Status.PolicySnapshot != "" {
		t.Errorf("policy snapshot = %q, %v; want dropped with both kinds in the bucket", obj.Status.PolicySnapshot, err)
	}
}

// A sharded kind's summary names its index, not a single object; another
// key of the kind is refused, and so is the index of a kind that is not
// sharded.
func TestSetResultRefOfANodesIndex(t *testing.T) {
	w := newWorld(t)
	c := w.cluster(t, "prod")
	clusters := w.s.Clusters()
	bucket := "cv-prod-1a2b3c4d-0123456789ab"
	ref := model.ResultRef{Kind: "nodes", Bucket: bucket, Key: "latest/nodes/index.json", ETag: `"i"`, Digest: "d", CollectedAt: resultsAt, Total: 1000}
	if err := clusters.SetResultRef(t.Context(), "acme", c.ID, ref); err != nil {
		t.Fatal(err)
	}
	row, err := clusters.Get(t.Context(), "acme", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := row.ResultRef("nodes"); got == nil || *got != ref {
		t.Fatalf("stored %+v, want %+v", got, ref)
	}
	for name, bad := range map[string]model.ResultRef{
		"the old single key":   {Kind: "nodes", Bucket: bucket, Key: "latest/nodes.json"},
		"a shard instead":      {Kind: "nodes", Bucket: bucket, Key: "latest/nodes/system.json"},
		"index of another one": {Kind: "violations", Bucket: bucket, Key: "latest/violations/index.json"},
	} {
		if err := clusters.SetResultRef(t.Context(), "acme", c.ID, bad); !errors.Is(err, repository.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}
