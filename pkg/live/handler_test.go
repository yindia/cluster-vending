package live

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// Over the wire: the API's client gets the reader's pages and its reasons;
// a reader that cannot be reached is LIVE_READER_UNAVAILABLE; a token from
// before a restart answers "restart the list"; only the live RPCs exist.
func TestHandlerOverTheWire(t *testing.T) {
	r, o, _ := newReader(t, DefaultLimits(), map[string]*fakeCluster{tenant + "/" + clusterID: many(10)})
	srv := httptest.NewServer(NewHandler(r))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, srv.Client())

	res, err := c.ListNodeGroupNodes(t.Context(), listReq(""))
	if err := Forward(err); err != nil || len(res.GetNodes()) != 4 || res.GetNextPageToken() == "" {
		t.Fatalf("page = %v, %v", res, err)
	}
	if _, err := c.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: clusterID}); err != nil {
		t.Fatal(err)
	}

	// Restart: a new process has none of the views.
	restarted, _, _ := newReader(t, DefaultLimits(), map[string]*fakeCluster{tenant + "/" + clusterID: many(10)})
	srv2 := httptest.NewServer(NewHandler(restarted))
	t.Cleanup(srv2.Close)
	_, err = NewClient(srv2.URL, srv2.Client()).ListNodeGroupNodes(t.Context(), listReq(res.GetNextPageToken()))
	if err := Forward(err); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), MessageRestart) {
		t.Fatalf("token after restart: %v", err)
	}

	o.err = &Error{Reason: ReasonNoLiveIdentity, Message: MessageNoLiveIdentity}
	r.store = newStore(r.lim) // drop the build so the next request opens the cluster
	_, err = c.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: clusterID})
	if err := Forward(err); connect.CodeOf(err) != connect.CodeUnavailable || ReasonOf(err) != ReasonNoLiveIdentity {
		t.Fatalf("no identity: %v (%q)", err, ReasonOf(err))
	}

	if _, err := c.(cloudv1connect.ClusterServiceClient).ListClusters(t.Context(), &cloudv1.ListClustersRequest{TenantId: tenant}); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("ListClusters on the live reader: %v", err)
	}

	metricsRes, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(metricsRes.Body)
	_ = metricsRes.Body.Close()
	if !strings.Contains(string(body), `cluster_vending_live_builds_total{kind="nodes",outcome="ok"}`) {
		t.Fatalf("metrics lack the builds: %s", body)
	}
	for _, want := range []string{
		`cluster_vending_live_rpc_requests_total{code="ok",rpc_method="ListNodeGroupNodes",rpc_service="cloud.v1.ClusterService",rpc_type="unary"}`,
		`cluster_vending_live_rpc_in_flight{rpc_method="GetNodeResources",rpc_service="cloud.v1.ClusterService",rpc_type="unary"} 0`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("metrics lack the reads %s: %s", want, body)
		}
	}

	srv.Close()
	_, err = c.GetNodeResources(t.Context(), &cloudv1.GetNodeResourcesRequest{TenantId: tenant, ClusterId: clusterID})
	if err := Forward(err); connect.CodeOf(err) != connect.CodeUnavailable || ReasonOf(err) != ReasonLiveReaderUnavailable {
		t.Fatalf("reader down: %v (%q)", err, ReasonOf(err))
	}
}

// The metrics listener serves the metrics and the probes, never the
// internal endpoint a scraper must not reach.
func TestMetricsHandlerServesNoRPC(t *testing.T) {
	srv := httptest.NewServer(NewMetricsHandler())
	defer srv.Close()
	for path, want := range map[string]int{"/metrics": http.StatusOK, "/healthz": http.StatusOK,
		"/cloud.v1.ClusterService/GetNodeResources": http.StatusNotFound} {
		res, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader("{}"))
		if path != "/cloud.v1.ClusterService/GetNodeResources" {
			res, err = srv.Client().Get(srv.URL + path)
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, res.StatusCode, want)
		}
	}
}
