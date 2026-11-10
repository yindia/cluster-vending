package aws_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/constant/aws"
)

// awsRegionPattern is the AWS region rule of the proto contract
// (ClusterSpec / NetworkSpec "aws.region" CEL rule). Every catalog region
// must pass it.
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(-gov|-iso[a-z]?)?-[a-z]+-[0-9]+$`)

var zoneIDPattern = regexp.MustCompile(`^([a-z]+[0-9])-az[0-9]+$`)

func TestRegionsAreWellFormed(t *testing.T) {
	regions := aws.Regions()
	if len(regions) != 34 {
		t.Fatalf("got %d regions, want 34 (AWS docs, commercial partition)", len(regions))
	}
	for _, r := range regions {
		if !awsRegionPattern.MatchString(string(r.Code)) {
			t.Errorf("%s does not match the contract's AWS region pattern", r.Code)
		}
		if r.Name == "" || r.Geography == "" {
			t.Errorf("%s: name and geography are required", r.Code)
		}
		if r.Partition != aws.PartitionAWS {
			t.Errorf("%s: partition %q, want %q", r.Code, r.Partition, aws.PartitionAWS)
		}
	}
	if !slices.IsSortedFunc(regions, func(a, b aws.Region) int { return strings.Compare(string(a.Code), string(b.Code)) }) {
		t.Error("Regions() must be sorted by code")
	}
}

func TestEveryRegionHasZonesWithItsPrefix(t *testing.T) {
	seen := map[aws.ZoneID]aws.RegionCode{}
	for _, r := range aws.Regions() {
		zones := aws.ZoneIDs(r.Code)
		if len(zones) < 3 {
			t.Errorf("%s: %d zones, every AWS region has at least 3", r.Code, len(zones))
		}
		prefix := ""
		for _, z := range zones {
			m := zoneIDPattern.FindStringSubmatch(string(z))
			if m == nil {
				t.Errorf("%s: malformed zone ID %q", r.Code, z)
				continue
			}
			if prefix == "" {
				prefix = m[1]
			}
			if m[1] != prefix {
				t.Errorf("%s: zone %s has prefix %s, want %s", r.Code, z, m[1], prefix)
			}
			if other, dup := seen[z]; dup {
				t.Errorf("zone %s listed in both %s and %s", z, other, r.Code)
			}
			seen[z] = r.Code
		}
	}
}

func TestZoneTableHasNoUnknownRegions(t *testing.T) {
	for _, code := range aws.ZoneRegions() {
		if _, ok := aws.LookupRegion(string(code)); !ok {
			t.Errorf("zone table has region %s that is not in the region table", code)
		}
	}
	if got, want := len(aws.ZoneRegions()), len(aws.Regions()); got != want {
		t.Errorf("zone table covers %d regions, region table has %d", got, want)
	}
}

func TestLookupRegion(t *testing.T) {
	tests := []struct {
		code      string
		wantOK    bool
		wantName  string
		wantOptIn bool
	}{
		{code: "us-east-1", wantOK: true, wantName: "US East (N. Virginia)"},
		{code: "ap-southeast-7", wantOK: true, wantName: "Asia Pacific (Thailand)", wantOptIn: true},
		{code: "sa-east-1", wantOK: true, wantName: "South America (São Paulo)"},
		{code: "us-east-9"},
		{code: ""},
		{code: "US-EAST-1"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			r, ok := aws.LookupRegion(tt.code)
			if ok != tt.wantOK {
				t.Fatalf("LookupRegion(%q) ok = %v, want %v", tt.code, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if r.Name != tt.wantName || r.OptInRequired != tt.wantOptIn {
				t.Errorf("got %+v, want name %q opt-in %v", r, tt.wantName, tt.wantOptIn)
			}
			if aws.IsRegion(tt.code) != ok {
				t.Errorf("IsRegion(%q) disagrees with LookupRegion", tt.code)
			}
		})
	}
}

func TestZoneIDs(t *testing.T) {
	tests := []struct {
		region aws.RegionCode
		want   []aws.ZoneID
	}{
		{aws.USEast1, []aws.ZoneID{"use1-az1", "use1-az2", "use1-az3", "use1-az4", "use1-az5", "use1-az6"}},
		{aws.CACentral1, []aws.ZoneID{"cac1-az1", "cac1-az2", "cac1-az4"}}, // no az3
		{aws.APNortheast1, []aws.ZoneID{"apne1-az1", "apne1-az2", "apne1-az3", "apne1-az4"}},
		{"us-east-9", nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.region), func(t *testing.T) {
			if got := aws.ZoneIDs(tt.region); !slices.Equal(got, tt.want) {
				t.Errorf("ZoneIDs(%s) = %v, want %v", tt.region, got, tt.want)
			}
		})
	}
}

func TestLookupZone(t *testing.T) {
	tests := []struct {
		id         string
		wantRegion aws.RegionCode
		wantOK     bool
	}{
		{id: "euw2-az4", wantRegion: aws.EUWest2, wantOK: true},
		{id: "mxc1-az3", wantRegion: aws.MXCentral1, wantOK: true},
		{id: "cac1-az3"},   // ca-central-1 has no az3
		{id: "us-east-1a"}, // a zone name, not an ID
		{id: ""},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			region, ok := aws.LookupZone(tt.id)
			if ok != tt.wantOK || region != tt.wantRegion {
				t.Errorf("LookupZone(%q) = %q, %v; want %q, %v", tt.id, region, ok, tt.wantRegion, tt.wantOK)
			}
		})
	}
}

func TestCallersCannotMutateTheCatalog(t *testing.T) {
	zones := aws.ZoneIDs(aws.USEast2)
	zones[0] = "tampered"
	if aws.ZoneIDs(aws.USEast2)[0] != "use2-az1" {
		t.Fatal("ZoneIDs returned the internal slice")
	}

	regions := aws.Regions()
	regions[0].Name = "tampered"
	if aws.Regions()[0].Name == "tampered" {
		t.Fatal("Regions returned the internal slice")
	}
}
