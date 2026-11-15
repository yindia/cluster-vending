package aws

import (
	"maps"
	"slices"
)

// Regions returns every Region, sorted by code; for the tests of the data.
func Regions() []Region { return slices.Clone(regions) }

// ZoneRegions returns the Regions that have zone data, sorted by code; for
// the tests of the data.
func ZoneRegions() []RegionCode { return slices.Sorted(maps.Keys(zoneIDs)) }
