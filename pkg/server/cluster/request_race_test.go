package cluster

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"

	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// racingClusters makes a create lose to a concurrent one: the request lookup
// before the insert finds nothing (the winner was not stored yet) and the
// insert is refused as a duplicate (it was, meanwhile).
type racingClusters struct {
	repository.Clusters
	hideOnce bool
}

func (r *racingClusters) GetByCreateRequest(ctx context.Context, tenantID, id string) (*model.Cluster, error) {
	if r.hideOnce {
		r.hideOnce = false
		return nil, repository.ErrNotFound
	}
	return r.Clusters.GetByCreateRequest(ctx, tenantID, id)
}

func (r *racingClusters) Create(context.Context, *model.Cluster) error {
	return fmt.Errorf("%w: cluster-request", repository.ErrAlreadyExists)
}

type racingStore struct {
	repository.Store
	clusters *racingClusters
}

func (s racingStore) Clusters() repository.Clusters { return s.clusters }

func raceWorld(t *testing.T) world {
	w := importWorld(t, terraformCluster())
	w.svc.deps.Store = racingStore{Store: w.svc.deps.Store, clusters: &racingClusters{Clusters: w.svc.deps.Store.Clusters()}}
	return w
}

func TestConcurrentRequestIDReuseForAnotherPayloadIsInvalidArgument(t *testing.T) {
	w := raceWorld(t)
	rid := uuid.NewString()
	// The winner is stored by the real store; the racing wrapper only refuses the loser.
	winner := createRequest(w, "managed-one")
	winner.RequestId = rid
	real := w.svc.deps.Store
	w.svc.deps.Store = real.(racingStore).Store
	if _, err := w.svc.CreateCluster(t.Context(), winner); err != nil {
		t.Fatal(err)
	}
	w.svc.deps.Store = real
	real.(racingStore).clusters.hideOnce = true

	other := createRequest(w, "managed-two")
	other.RequestId = rid
	_, err := w.svc.CreateCluster(t.Context(), other)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("concurrent reuse of a request_id for another payload: %v", err)
	}
}

func TestRequestWinnerOfAnIdenticalRetryIsTheIdempotentResult(t *testing.T) {
	w := raceWorld(t)
	rid := uuid.NewString()
	winner := createRequest(w, "managed-one")
	winner.RequestId = rid
	real := w.svc.deps.Store
	w.svc.deps.Store = real.(racingStore).Store
	created, err := w.svc.CreateCluster(t.Context(), winner)
	if err != nil {
		t.Fatal(err)
	}
	// The loser's row: another ID, the same request.
	loser := &model.Cluster{ID: uuid.NewString(), TenantID: worldTenant, CreateRequestID: &rid}
	dup := fmt.Errorf("%w: cluster-request", repository.ErrAlreadyExists)
	got, err := w.svc.requestWinner(t.Context(), loser, dup, func(*model.Cluster) bool { return true })
	if err != nil || got == nil || got.ID != created.GetCluster().GetId() {
		t.Fatalf("winner = %v, %v", got, err)
	}
	if got, err := w.svc.requestWinner(t.Context(), loser, dup, func(*model.Cluster) bool { return false }); got != nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("different payload: %v, %v", got, err)
	}
	// Not a request ID failure: left to the caller.
	if got, err := w.svc.requestWinner(t.Context(), loser, repository.ErrConflict, func(*model.Cluster) bool { return true }); got != nil || err != nil {
		t.Fatalf("unrelated error: %v, %v", got, err)
	}
}

func TestCreateErrorDistinguishesConflicts(t *testing.T) {
	row := &model.Cluster{NetworkID: "n1", Name: "c", TenantID: "t"}
	deleting := fmt.Errorf("%w: %w: network n", repository.ErrConflict, cloudstore.ErrNetworkDeleting)
	if code := connect.CodeOf(createError(deleting, row)); code != connect.CodeFailedPrecondition {
		t.Errorf("network being deleted: %v", code)
	}
	other := fmt.Errorf("%w: the object kept changing", repository.ErrConflict)
	if code := connect.CodeOf(createError(other, row)); code != connect.CodeAborted {
		t.Errorf("other conflict: %v", code)
	}
}
