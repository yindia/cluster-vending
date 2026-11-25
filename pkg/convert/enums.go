// Package convert translates between the cloud.v1 contract (the protobuf
// messages the API speaks and the reconcilers work on) and the
// cloud.cluster-vending.io/v1alpha1 kinds (what the management cluster
// stores). It has no client: every function is a pure mapping.
//
// A spec converts both ways without loss: contract -> kind -> contract
// gives the message back. What a kind's typed spec has no field for is
// returned as a remainder (a contract message holding only those fields),
// for the caller to keep next to the object and hand back; nothing is
// dropped. A status converts one way, contract -> kind, for `kubectl get`.
//
// Two things are references, not values, and are passed in: the names of
// the objects a spec refers to (its account, its network), which the
// contract names by ID.
package convert

import (
	"math"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// camel is the kind-side spelling of a contract enum value: its name
// without prefix, in CamelCase (NAT_GATEWAY_MODE_PER_ZONE -> "PerZone").
// The unspecified value (0) and numbers the enum does not have are "".
func camel(e protoreflect.Enum, prefix string) string {
	if e.Number() == 0 {
		return ""
	}
	value := e.Descriptor().Values().ByNumber(e.Number())
	if value == nil {
		return ""
	}
	return camelName(string(value.Name()), prefix)
}

func camelName(name, prefix string) string {
	var b strings.Builder
	for _, word := range strings.Split(strings.TrimPrefix(name, prefix), "_") {
		if word == "" {
			continue
		}
		b.WriteString(word[:1] + strings.ToLower(word[1:]))
	}
	return b.String()
}

// uncamel is the number of the enum value camel spells s; 0 (unspecified)
// for "" and for a spelling the enum does not have.
func uncamel(desc protoreflect.EnumDescriptor, prefix, s string) protoreflect.EnumNumber {
	if s == "" {
		return 0
	}
	values := desc.Values()
	for i := range values.Len() {
		v := values.Get(i)
		if v.Number() != 0 && camelName(string(v.Name()), prefix) == s {
			return v.Number()
		}
	}
	return 0
}

// awsName is the kind-side spelling of a contract enum value that the kind
// spells as AWS does: its (cloud.v1.crossplane) aws name. "" for the
// unspecified value and for values without one.
func awsName(e protoreflect.Enum) string {
	if e.Number() == 0 {
		return ""
	}
	return awsNameOf(e.Descriptor().Values().ByNumber(e.Number()))
}

func awsNameOf(value protoreflect.EnumValueDescriptor) string {
	if value == nil {
		return ""
	}
	names, ok := proto.GetExtension(value.Options(), cloudv1.E_Crossplane).(*cloudv1.CrossplaneValues)
	if !ok {
		return ""
	}
	return names.GetAws()
}

// fromAWSName is the number of the enum value AWS spells s; 0 for "" and
// for a spelling the enum does not have.
func fromAWSName(desc protoreflect.EnumDescriptor, s string) protoreflect.EnumNumber {
	if s == "" {
		return 0
	}
	values := desc.Values()
	for i := range values.Len() {
		if v := values.Get(i); v.Number() != 0 && awsNameOf(v) == s {
			return v.Number()
		}
	}
	return 0
}

// Enum name prefixes of the contract.
const (
	prefixSubnetRole     = "SUBNET_ROLE_"
	prefixNAT            = "NAT_GATEWAY_MODE_"
	prefixFlowLog        = "FLOW_LOG_TRAFFIC_TYPE_"
	prefixPolicyScope    = "ACCESS_POLICY_SCOPE_"
	prefixReleaseChannel = "RELEASE_CHANNEL_"
	prefixTier           = "CLUSTER_" // CLUSTER_TIER_0 -> "Tier0"
	prefixUpgradeMode    = "UPGRADE_MODE_"
	prefixWeekday        = "WEEKDAY_"
	prefixNetworkState   = "NETWORK_STATE_"
	prefixClusterState   = "CLUSTER_STATE_"
	prefixImportPhase    = "IMPORT_PHASE_"
	prefixDisposition    = "DISPOSITION_"
)

// toInt32 is u as the kinds' int32; the contract's sizes and counts are
// far below the limit, a larger value is cut to it.
func toInt32(u uint32) int32 {
	if u > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(u)
}

// toUint32 is i as the contract's uint32; a negative value is 0.
func toUint32(i int32) uint32 {
	if i < 0 {
		return 0
	}
	return uint32(i)
}
