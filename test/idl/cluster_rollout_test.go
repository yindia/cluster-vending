package idl_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/encoding/protojson"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// release_channel and tier take their defined values, or none.
func TestReleaseChannelAndTierValidation(t *testing.T) {
	type spec = cloudv1.ClusterSpec
	type req = cloudv1.CreateClusterRequest
	for _, tt := range []struct {
		name    string
		set     func(*spec)
		wantErr bool
	}{
		{name: "neither set", set: func(*spec) {}},
		{name: "alpha, tier 2", set: func(s *spec) {
			s.ReleaseChannel, s.Tier = cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA, cloudv1.ClusterTier_CLUSTER_TIER_2
		}},
		{name: "beta, tier 1", set: func(s *spec) {
			s.ReleaseChannel, s.Tier = cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, cloudv1.ClusterTier_CLUSTER_TIER_1
		}},
		{name: "stable, tier 0", set: func(s *spec) {
			s.ReleaseChannel, s.Tier = cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE, cloudv1.ClusterTier_CLUSTER_TIER_0
		}},
		{name: "channel only", set: func(s *spec) { s.ReleaseChannel = cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE }},
		{name: "tier only", set: func(s *spec) { s.Tier = cloudv1.ClusterTier_CLUSTER_TIER_0 }},
		{name: "undefined channel", wantErr: true, set: func(s *spec) { s.ReleaseChannel = cloudv1.ReleaseChannel(99) }},
		{name: "undefined tier", wantErr: true, set: func(s *spec) { s.Tier = cloudv1.ClusterTier(99) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := protovalidate.Validate(mutate(func(s *spec, _ *req) { tt.set(s) }))
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

// The order the comments promise is the order of the numbers: a rollout
// that sorts by them goes ALPHA, BETA, STABLE, and within a channel from
// the highest tier number to the lowest.
func TestReleaseOrderIsInTheNumbers(t *testing.T) {
	if cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA >= cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA ||
		cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA >= cloudv1.ReleaseChannel_RELEASE_CHANNEL_STABLE {
		t.Error("channels are not numbered in release order: ALPHA, BETA, STABLE")
	}
	if cloudv1.ClusterTier_CLUSTER_TIER_0 >= cloudv1.ClusterTier_CLUSTER_TIER_1 ||
		cloudv1.ClusterTier_CLUSTER_TIER_1 >= cloudv1.ClusterTier_CLUSTER_TIER_2 {
		t.Error("tiers are not numbered by sensitivity: 0, 1, 2")
	}
}

// Both fields survive what a create does to a spec (resolving its
// defaults) and the JSON the spec is stored as, so Get returns them.
func TestReleaseChannelAndTierAreStored(t *testing.T) {
	in := validCreate().GetCluster().GetSpec()
	in.ReleaseChannel, in.Tier = cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA, cloudv1.ClusterTier_CLUSTER_TIER_1
	resolved, err := corecluster.ResolveSpec(in, []string{"use1-az1", "use1-az2"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := protojson.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	stored := &cloudv1.ClusterSpec{}
	if err := protojson.Unmarshal(raw, stored); err != nil {
		t.Fatal(err)
	}
	if stored.GetReleaseChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA || stored.GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_1 {
		t.Errorf("stored spec has channel %s and tier %s", stored.GetReleaseChannel(), stored.GetTier())
	}
}
