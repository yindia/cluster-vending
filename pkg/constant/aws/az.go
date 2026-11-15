package aws

import (
	"slices"
)

// ZoneID is an Availability Zone ID, e.g. "use1-az1". Unlike a zone name
// ("us-east-1a"), an AZ ID is the same physical location in every AWS
// account, so the platform stores and passes zone IDs (Crossplane
// Subnet.forProvider.availabilityZoneId) and never zone names.
type ZoneID string

// zoneIDs lists the AZ IDs of every Region in the catalog, in AZ ID order.
// Gaps are real: ca-central-1 has no cac1-az3. us-east-1's seventh zone
// (Maryland, "coming in 2026") is not listed until AWS publishes its ID.
var zoneIDs = map[RegionCode][]ZoneID{
	AFSouth1:     {"afs1-az1", "afs1-az2", "afs1-az3"},
	APEast1:      {"ape1-az1", "ape1-az2", "ape1-az3"},
	APEast2:      {"ape2-az1", "ape2-az2", "ape2-az3"},
	APNortheast1: {"apne1-az1", "apne1-az2", "apne1-az3", "apne1-az4"},
	APNortheast2: {"apne2-az1", "apne2-az2", "apne2-az3", "apne2-az4"},
	APNortheast3: {"apne3-az1", "apne3-az2", "apne3-az3"},
	APSouth1:     {"aps1-az1", "aps1-az2", "aps1-az3"},
	APSouth2:     {"aps2-az1", "aps2-az2", "aps2-az3"},
	APSoutheast1: {"apse1-az1", "apse1-az2", "apse1-az3"},
	APSoutheast2: {"apse2-az1", "apse2-az2", "apse2-az3"},
	APSoutheast3: {"apse3-az1", "apse3-az2", "apse3-az3"},
	APSoutheast4: {"apse4-az1", "apse4-az2", "apse4-az3"},
	APSoutheast5: {"apse5-az1", "apse5-az2", "apse5-az3"},
	APSoutheast6: {"apse6-az1", "apse6-az2", "apse6-az3"},
	APSoutheast7: {"apse7-az1", "apse7-az2", "apse7-az3"},
	CACentral1:   {"cac1-az1", "cac1-az2", "cac1-az4"},
	CAWest1:      {"caw1-az1", "caw1-az2", "caw1-az3"},
	EUCentral1:   {"euc1-az1", "euc1-az2", "euc1-az3"},
	EUCentral2:   {"euc2-az1", "euc2-az2", "euc2-az3"},
	EUNorth1:     {"eun1-az1", "eun1-az2", "eun1-az3"},
	EUSouth1:     {"eus1-az1", "eus1-az2", "eus1-az3"},
	EUSouth2:     {"eus2-az1", "eus2-az2", "eus2-az3"},
	EUWest1:      {"euw1-az1", "euw1-az2", "euw1-az3"},
	EUWest2:      {"euw2-az1", "euw2-az2", "euw2-az3", "euw2-az4"},
	EUWest3:      {"euw3-az1", "euw3-az2", "euw3-az3"},
	ILCentral1:   {"ilc1-az1", "ilc1-az2", "ilc1-az3"},
	MECentral1:   {"mec1-az1", "mec1-az2", "mec1-az3"},
	MESouth1:     {"mes1-az1", "mes1-az2", "mes1-az3"},
	MXCentral1:   {"mxc1-az1", "mxc1-az2", "mxc1-az3"},
	SAEast1:      {"sae1-az1", "sae1-az2", "sae1-az3"},
	USEast1:      {"use1-az1", "use1-az2", "use1-az3", "use1-az4", "use1-az5", "use1-az6"},
	USEast2:      {"use2-az1", "use2-az2", "use2-az3"},
	USWest1:      {"usw1-az1", "usw1-az2", "usw1-az3"},
	USWest2:      {"usw2-az1", "usw2-az2", "usw2-az3", "usw2-az4"},
}

// zoneRegion maps every AZ ID back to its Region.
var zoneRegion = func() map[ZoneID]RegionCode {
	m := make(map[ZoneID]RegionCode)
	for region, zones := range zoneIDs {
		for _, z := range zones {
			m[z] = region
		}
	}
	return m
}()

// ZoneIDs returns the AZ IDs of a Region in AZ ID order, or nil for an
// unknown Region.
func ZoneIDs(region RegionCode) []ZoneID {
	return slices.Clone(zoneIDs[region])
}

// LookupZone returns the Region an AZ ID belongs to.
func LookupZone(id string) (RegionCode, bool) {
	region, ok := zoneRegion[ZoneID(id)]
	return region, ok
}
