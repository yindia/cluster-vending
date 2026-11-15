// Package aws is the static catalog of AWS Regions and Availability Zones
// the platform knows about (commercial "aws" partition).
//
// Source: AWS documentation, "AWS Regions" and "AWS Availability Zones"
// (docs.aws.amazon.com/global-infrastructure/latest/regions/), checked
// 2026-11-10. To refresh, compare with:
//
//	aws ec2 describe-regions --all-regions
//	aws ec2 describe-availability-zones --all-availability-zones \
//	  --filters Name=zone-type,Values=availability-zone --region <code>
//
// Not covered: AWS GovCloud (US) and China, which are separate partitions,
// and constrained zones. An AZ ID listed here can still be unavailable to a
// particular account (e.g. newer accounts get two of us-west-1's zones).
//
// The tables are unexported; every exported function returns a copy, so
// callers cannot change the catalog.
package aws

import (
	"slices"
	"strings"
)

// PartitionAWS is the commercial AWS partition.
const PartitionAWS = "aws"

// RegionCode is an AWS Region code, e.g. "us-east-1".
type RegionCode string

// Region codes of the commercial partition.
const (
	AFSouth1     RegionCode = "af-south-1"
	APEast1      RegionCode = "ap-east-1"
	APEast2      RegionCode = "ap-east-2"
	APNortheast1 RegionCode = "ap-northeast-1"
	APNortheast2 RegionCode = "ap-northeast-2"
	APNortheast3 RegionCode = "ap-northeast-3"
	APSouth1     RegionCode = "ap-south-1"
	APSouth2     RegionCode = "ap-south-2"
	APSoutheast1 RegionCode = "ap-southeast-1"
	APSoutheast2 RegionCode = "ap-southeast-2"
	APSoutheast3 RegionCode = "ap-southeast-3"
	APSoutheast4 RegionCode = "ap-southeast-4"
	APSoutheast5 RegionCode = "ap-southeast-5"
	APSoutheast6 RegionCode = "ap-southeast-6"
	APSoutheast7 RegionCode = "ap-southeast-7"
	CACentral1   RegionCode = "ca-central-1"
	CAWest1      RegionCode = "ca-west-1"
	EUCentral1   RegionCode = "eu-central-1"
	EUCentral2   RegionCode = "eu-central-2"
	EUNorth1     RegionCode = "eu-north-1"
	EUSouth1     RegionCode = "eu-south-1"
	EUSouth2     RegionCode = "eu-south-2"
	EUWest1      RegionCode = "eu-west-1"
	EUWest2      RegionCode = "eu-west-2"
	EUWest3      RegionCode = "eu-west-3"
	ILCentral1   RegionCode = "il-central-1"
	MECentral1   RegionCode = "me-central-1"
	MESouth1     RegionCode = "me-south-1"
	MXCentral1   RegionCode = "mx-central-1"
	SAEast1      RegionCode = "sa-east-1"
	USEast1      RegionCode = "us-east-1"
	USEast2      RegionCode = "us-east-2"
	USWest1      RegionCode = "us-west-1"
	USWest2      RegionCode = "us-west-2"
)

// Region describes one AWS Region.
type Region struct {
	Code RegionCode
	// Name is the AWS long name, e.g. "US East (N. Virginia)".
	Name string
	// Geography is where the Region's infrastructure is, e.g. "Japan".
	Geography string
	Partition string
	// OptInRequired is true for Regions introduced after March 20, 2019:
	// the account must enable them before use.
	OptInRequired bool
}

// regions is sorted by code.
var regions = []Region{
	{AFSouth1, "Africa (Cape Town)", "South Africa", PartitionAWS, true},
	{APEast1, "Asia Pacific (Hong Kong)", "Hong Kong", PartitionAWS, true},
	{APEast2, "Asia Pacific (Taipei)", "Taiwan", PartitionAWS, true},
	{APNortheast1, "Asia Pacific (Tokyo)", "Japan", PartitionAWS, false},
	{APNortheast2, "Asia Pacific (Seoul)", "South Korea", PartitionAWS, false},
	{APNortheast3, "Asia Pacific (Osaka)", "Japan", PartitionAWS, false},
	{APSouth1, "Asia Pacific (Mumbai)", "India", PartitionAWS, false},
	{APSouth2, "Asia Pacific (Hyderabad)", "India", PartitionAWS, true},
	{APSoutheast1, "Asia Pacific (Singapore)", "Singapore", PartitionAWS, false},
	{APSoutheast2, "Asia Pacific (Sydney)", "Australia", PartitionAWS, false},
	{APSoutheast3, "Asia Pacific (Jakarta)", "Indonesia", PartitionAWS, true},
	{APSoutheast4, "Asia Pacific (Melbourne)", "Australia", PartitionAWS, true},
	{APSoutheast5, "Asia Pacific (Malaysia)", "Malaysia", PartitionAWS, true},
	{APSoutheast6, "Asia Pacific (New Zealand)", "New Zealand", PartitionAWS, true},
	{APSoutheast7, "Asia Pacific (Thailand)", "Thailand", PartitionAWS, true},
	{CACentral1, "Canada (Central)", "Canada", PartitionAWS, false},
	{CAWest1, "Canada West (Calgary)", "Canada", PartitionAWS, true},
	{EUCentral1, "Europe (Frankfurt)", "Germany", PartitionAWS, false},
	{EUCentral2, "Europe (Zurich)", "Switzerland", PartitionAWS, true},
	{EUNorth1, "Europe (Stockholm)", "Sweden", PartitionAWS, false},
	{EUSouth1, "Europe (Milan)", "Italy", PartitionAWS, true},
	{EUSouth2, "Europe (Spain)", "Spain", PartitionAWS, true},
	{EUWest1, "Europe (Ireland)", "Ireland", PartitionAWS, false},
	{EUWest2, "Europe (London)", "United Kingdom", PartitionAWS, false},
	{EUWest3, "Europe (Paris)", "France", PartitionAWS, false},
	{ILCentral1, "Israel (Tel Aviv)", "Israel", PartitionAWS, true},
	{MECentral1, "Middle East (UAE)", "United Arab Emirates", PartitionAWS, true},
	{MESouth1, "Middle East (Bahrain)", "Bahrain", PartitionAWS, true},
	{MXCentral1, "Mexico (Central)", "Mexico", PartitionAWS, true},
	{SAEast1, "South America (São Paulo)", "Brazil", PartitionAWS, false},
	{USEast1, "US East (N. Virginia)", "United States of America", PartitionAWS, false},
	{USEast2, "US East (Ohio)", "United States of America", PartitionAWS, false},
	{USWest1, "US West (N. California)", "United States of America", PartitionAWS, false},
	{USWest2, "US West (Oregon)", "United States of America", PartitionAWS, false},
}

// LookupRegion returns the Region with the given code. Codes are
// case-sensitive, as in AWS.
func LookupRegion(code string) (Region, bool) {
	i, found := slices.BinarySearchFunc(regions, RegionCode(code), func(r Region, c RegionCode) int {
		return strings.Compare(string(r.Code), string(c))
	})
	if !found {
		return Region{}, false
	}
	return regions[i], true
}

// IsRegion reports whether code is a known Region code.
func IsRegion(code string) bool {
	_, ok := LookupRegion(code)
	return ok
}
