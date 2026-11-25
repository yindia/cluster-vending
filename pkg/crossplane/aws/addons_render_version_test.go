package aws_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// addonRenderGolden pairs the render of every add-on (all add-ons the API
// accepts, on the tainted, labelled default pool of clusterPlan) with the
// RenderVersion it shipped in.
//
// The add-ons are Build items: the code-change fingerprint
// (sequence.Fingerprints) counts them by name only, so a change to what an
// add-on renders (its configurationValues, say) is applied to existing
// clusters only through a RenderVersion bump. When this test fails because
// the add-on render changed, bump crossplaneaws.RenderVersion (with a line
// in its history) and update both values here.
var addonRenderGolden = struct {
	version int
	sha256  string
}{version: 10, sha256: "32359f804835a80e43305702c4d589a92b046f94e956d4ff982f0782c83ec3c3"}

func TestAddonRenderChangeBumpsRenderVersion(t *testing.T) {
	var addons []*awseks.Addon
	for _, o := range crossplaneaws.ClusterResources(planWithAddons(nil)) {
		if addon, ok := o.(*awseks.Addon); ok {
			addons = append(addons, addon)
		}
	}
	if len(addons) != len(allAddons) {
		t.Fatalf("rendered %d add-ons, want %d", len(addons), len(allAddons))
	}
	raw, err := json.Marshal(addons)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if got != addonRenderGolden.sha256 || crossplaneaws.RenderVersion != addonRenderGolden.version {
		t.Errorf("add-on render %s at RenderVersion %d, golden %s at %d: an add-on render change must bump "+
			"RenderVersion (existing clusters see it only through the bump); update addonRenderGolden with it",
			got, crossplaneaws.RenderVersion, addonRenderGolden.sha256, addonRenderGolden.version)
	}
}
