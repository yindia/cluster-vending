package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/cluster"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

const inferCluster = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"

// updateCluster answers GetCluster with current and records UpdateCluster.
type updateCluster struct {
	cloudv1connect.ClusterServiceClient
	current *cloudv1.Cluster
	// requested: the cluster as its client asked for it (as_requested);
	// nil: the server has no such view and answers the resolved one.
	requested *cloudv1.Cluster
	updated   *cloudv1.UpdateClusterRequest
}

func (s *updateCluster) GetCluster(_ context.Context, req *cloudv1.GetClusterRequest) (*cloudv1.GetClusterResponse, error) {
	if req.GetAsRequested() && s.requested != nil {
		return &cloudv1.GetClusterResponse{Cluster: s.requested, AsRequested: true}, nil
	}
	return &cloudv1.GetClusterResponse{Cluster: s.current}, nil
}

func (s *updateCluster) UpdateCluster(_ context.Context, req *cloudv1.UpdateClusterRequest) (*cloudv1.UpdateClusterResponse, error) {
	s.updated = req
	return &cloudv1.UpdateClusterResponse{Cluster: s.current}, nil
}

// storedCluster is the cluster as GetCluster returns it: the stored spec,
// with the defaults the server filled in.
func storedCluster() *cloudv1.Cluster {
	return &cloudv1.Cluster{TenantId: "acme", Id: inferCluster, Name: "prod-eks", Etag: "etag-1", Labels: map[string]string{"env": "prod"},
		Spec: &cloudv1.ClusterSpec{Region: "us-west-1",
			ControlPlane:   &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.36"},
			ReleaseChannel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, Tier: cloudv1.ClusterTier_CLUSTER_TIER_2,
			Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Irsa: proto.Bool(true),
				Addons: mergedAddons("metrics-server")}}}}
}

// mergedAddons are the add-ons as the server stores them: the defaults
// with the named ones.
func mergedAddons(names ...string) []*cloudv1.AwsAddon {
	var in []*cloudv1.AwsAddon
	for _, n := range names {
		in = append(in, &cloudv1.AwsAddon{Name: n})
	}
	return cluster.MergeAddons(in)
}

func writeManifest(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const clusterHead = `apiVersion: cluster-vending.io/v1
kind: Cluster
metadata: {name: prod-eks, tenant: acme, id: ` + inferCluster + `}
`

func TestClusterUpdateInfersTheMask(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		opts     Options
		wantMask []string
		wantETag string
		wantErr  string
		progress []string
		noUpdate bool
	}{
		{name: "a channel change", wantMask: []string{"spec.release_channel"}, wantETag: "etag-1",
			manifest: clusterHead + "spec: {region: us-west-1, releaseChannel: RELEASE_CHANNEL_ALPHA}\n",
			progress: []string{"spec.release_channel: Beta → Alpha"}},
		{name: "a maintenance window added", wantMask: []string{"spec.maintenance_window"}, wantETag: "etag-1",
			manifest: clusterHead + "spec: {region: us-west-1, maintenanceWindow: {days: [WEEKDAY_SATURDAY], start: \"02:00\", duration: 4h}}\n",
			progress: []string{"spec.maintenance_window: <none> → Sat 02:00 UTC for 4h"}},
		{name: "labels changed", wantMask: []string{"labels"}, wantETag: "etag-1",
			manifest: "apiVersion: cluster-vending.io/v1\nkind: Cluster\nmetadata: {name: prod-eks, tenant: acme, id: " + inferCluster +
				", labels: {env: prod, team: a}}\nspec: {region: us-west-1}\n"},
		{name: "nothing changed: the add-ons the server merges in are not a change", noUpdate: true,
			manifest: clusterHead + "spec: {region: us-west-1, controlPlane: {kubernetesVersion: \"1.36\"}, aws: {addons: [{name: metrics-server}]}}\n",
			progress: []string{"nothing to change"}},
		{name: "the region cannot change", noUpdate: true, wantErr: "spec.region cannot be changed",
			manifest: clusterHead + "spec: {region: us-east-1}\n"},
		{name: "--etag wins over the one read", wantMask: []string{"spec.tier"}, wantETag: "mine",
			opts:     Options{ETag: "mine"},
			manifest: clusterHead + "spec: {region: us-west-1, tier: CLUSTER_TIER_1}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &updateCluster{current: storedCluster()}
			var out, progress bytes.Buffer
			o := tt.opts
			o.Input, o.Progress = writeManifest(t, tt.manifest), &progress

			err := Run(t.Context(), ClusterUpdate, Clients{Cluster: api}, o, &out, OutputTable)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if tt.noUpdate {
				if api.updated != nil {
					t.Errorf("UpdateCluster called: %v", api.updated)
				}
			} else {
				if api.updated == nil {
					t.Fatal("UpdateCluster not called")
				}
				if got := api.updated.GetUpdateMask().GetPaths(); !slices.Equal(got, tt.wantMask) {
					t.Errorf("mask = %v, want %v", got, tt.wantMask)
				}
				if got := api.updated.GetCluster().GetEtag(); got != tt.wantETag {
					t.Errorf("etag = %q, want %q", got, tt.wantETag)
				}
			}
			t.Logf("progress:\n%s", progress.String())
			for _, p := range tt.progress {
				if !strings.Contains(progress.String(), p) {
					t.Errorf("progress %q, want %q", progress.String(), p)
				}
			}
		})
	}
}

// updateNetwork answers GetNetwork with current and records UpdateNetwork.
type updateNetwork struct {
	cloudv1connect.NetworkServiceClient
	current *cloudv1.Network
	updated *cloudv1.UpdateNetworkRequest
}

func (s *updateNetwork) GetNetwork(context.Context, *cloudv1.GetNetworkRequest) (*cloudv1.GetNetworkResponse, error) {
	return &cloudv1.GetNetworkResponse{Network: s.current}, nil
}

func (s *updateNetwork) UpdateNetwork(_ context.Context, req *cloudv1.UpdateNetworkRequest) (*cloudv1.UpdateNetworkResponse, error) {
	s.updated = req
	return &cloudv1.UpdateNetworkResponse{Network: s.current}, nil
}

func TestNetworkUpdateInfersTheMask(t *testing.T) {
	const id = "01a1dbab-1b59-7140-9a3c-1f7a49ea1960"
	api := &updateNetwork{current: &cloudv1.Network{TenantId: "acme", Id: id, Name: "prod", Etag: "n-1",
		Spec: &cloudv1.NetworkSpec{Region: "us-west-1"}}}
	var out, progress bytes.Buffer
	manifest := writeManifest(t, "apiVersion: cluster-vending.io/v1\nkind: Network\nmetadata: {name: prod, tenant: acme, id: "+id+
		"}\nspec: {region: us-west-1, flowLogs: {enabled: true}}\n")

	err := Run(t.Context(), NetworkUpdate, Clients{Network: api}, Options{Input: manifest, Progress: &progress}, &out, OutputTable)

	if err != nil {
		t.Fatal(err)
	}
	if got := api.updated.GetUpdateMask().GetPaths(); !slices.Equal(got, []string{"spec.flow_logs"}) || api.updated.GetNetwork().GetEtag() != "n-1" {
		t.Errorf("mask %v, etag %q", got, api.updated.GetNetwork().GetEtag())
	}
	if !strings.Contains(progress.String(), "spec.flow_logs:") {
		t.Errorf("progress = %q", progress.String())
	}
}

// The main use: the whole manifest the cluster was created from, applied
// again. Compared with what was asked for (not the resolved spec, whose
// defaults it never states), it changes nothing, immutable fields and all;
// with only its release channel edited it changes exactly that.
func TestClusterUpdateReapplyingTheCreateManifest(t *testing.T) {
	const example = "../../examples/cluster.yaml"
	raw, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(example, KindCluster)
	if err != nil {
		t.Fatal(err)
	}
	created, err := m.Cluster()
	if err != nil {
		t.Fatal(err)
	}
	created.Id, created.Etag = inferCluster, "etag-1"
	edited := strings.Replace(string(raw), "\nspec:\n", "\nspec:\n  releaseChannel: RELEASE_CHANNEL_ALPHA\n", 1)
	tests := []struct {
		name     string
		manifest string
		wantMask []string
	}{
		{name: "the same manifest: nothing to change", manifest: string(raw)},
		{name: "only the release channel edited", manifest: edited, wantMask: []string{"spec.release_channel"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &updateCluster{current: storedCluster(), requested: created}
			var out, progress bytes.Buffer

			err := Run(t.Context(), ClusterUpdate, Clients{Cluster: api},
				Options{Input: writeManifest(t, tt.manifest), ID: inferCluster, Progress: &progress}, &out, OutputTable)

			if err != nil {
				t.Fatalf("%v\n%s", err, progress.String())
			}
			t.Logf("progress:\n%s", progress.String())
			if tt.wantMask == nil {
				if api.updated != nil || !strings.Contains(progress.String(), "nothing to change") {
					t.Errorf("updated %v; progress %q", api.updated, progress.String())
				}
				return
			}
			if got := api.updated.GetUpdateMask().GetPaths(); !slices.Equal(got, tt.wantMask) || api.updated.GetCluster().GetEtag() != "etag-1" {
				t.Errorf("mask %v, etag %q; want %v", got, api.updated.GetCluster().GetEtag(), tt.wantMask)
			}
		})
	}
}
