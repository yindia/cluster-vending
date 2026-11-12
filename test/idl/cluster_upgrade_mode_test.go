package idl_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/encoding/protojson"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// upgrade_mode takes its defined values, or none.
func TestUpgradeModeValidation(t *testing.T) {
	type spec = cloudv1.ClusterSpec
	type req = cloudv1.CreateClusterRequest
	for name, tt := range map[string]struct {
		mode    cloudv1.UpgradeMode
		wantErr bool
	}{
		"unset":     {mode: cloudv1.UpgradeMode_UPGRADE_MODE_UNSPECIFIED},
		"manual":    {mode: cloudv1.UpgradeMode_UPGRADE_MODE_MANUAL},
		"automatic": {mode: cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC},
		"undefined": {mode: cloudv1.UpgradeMode(99), wantErr: true},
	} {
		err := protovalidate.Validate(mutate(func(s *spec, _ *req) { s.UpgradeMode = tt.mode }))
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: Validate = %v, wantErr %t", name, err, tt.wantErr)
		}
	}
}

// The mode survives what a create does to a spec and its storage: a
// chosen mode is kept, an unset one is stored as MANUAL, and JSON uses the
// enum's name.
func TestUpgradeModeIsStoredAndReturned(t *testing.T) {
	for in, want := range map[cloudv1.UpgradeMode]cloudv1.UpgradeMode{
		cloudv1.UpgradeMode_UPGRADE_MODE_UNSPECIFIED: cloudv1.UpgradeMode_UPGRADE_MODE_MANUAL,
		cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC:   cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC,
	} {
		s := mutate(func(*cloudv1.ClusterSpec, *cloudv1.CreateClusterRequest) {}).GetCluster().GetSpec()
		s.UpgradeMode = in
		resolved, err := corecluster.ResolveSpec(s, []string{"use1-az1", "use1-az2"})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := protojson.Marshal(resolved)
		if err != nil {
			t.Fatal(err)
		}
		back := &cloudv1.ClusterSpec{}
		if err := protojson.Unmarshal(raw, back); err != nil {
			t.Fatal(err)
		}
		if back.GetUpgradeMode() != want {
			t.Errorf("mode %s stored as %s, want %s (%s)", in, back.GetUpgradeMode(), want, raw)
		}
	}
}
