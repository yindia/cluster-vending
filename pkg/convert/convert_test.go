package convert_test

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// variants is how many differently filled messages a round trip is tried
// with: more than the longest enum has values, so every value is used.
const variants = 48

// fill sets every field of m, recursively, to a value that depends on k:
// enums walk through their values (never the unspecified one), booleans
// alternate, lists get two elements, and now and then an optional scalar
// or a message is left unset, so both a set and an unset form of every
// field is seen across the variants. A field added to the contract is
// filled too: a conversion that does not carry it fails the round trip.
func fill(m protoreflect.Message, k int) {
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		n := k + i
		switch {
		case fd.IsMap():
			m.Mutable(fd).Map().Set(protoreflect.ValueOfString(fmt.Sprintf("k%d", n)).MapKey(), scalar(fd.MapValue(), n))
		case fd.IsList():
			list := m.Mutable(fd).List()
			for j := range 2 {
				if fd.Message() != nil {
					fill(list.AppendMutable().Message(), n+j)
				} else {
					list.Append(scalar(fd, n+j))
				}
			}
		case fd.Message() != nil:
			if n%7 != 6 {
				fill(m.Mutable(fd).Message(), n)
			}
		case fd.HasPresence() && n%5 == 4:
			// An optional scalar left unset.
		default:
			m.Set(fd, scalar(fd, n))
		}
	}
}

// scalar is a non-zero value for a scalar field.
func scalar(fd protoreflect.FieldDescriptor, n int) protoreflect.Value {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(fmt.Sprintf("%s-%d", fd.Name(), n))
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(n%2 == 0)
	case protoreflect.Uint32Kind:
		return protoreflect.ValueOfUint32(uint32(n%1000) + 1)
	case protoreflect.EnumKind:
		values := fd.Enum().Values()
		return protoreflect.ValueOfEnum(values.Get(1 + n%(values.Len()-1)).Number())
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes(fmt.Appendf(nil, "bytes-%d", n))
	case protoreflect.Int32Kind:
		return protoreflect.ValueOfInt32(int32(n%1000) + 1)
	case protoreflect.Int64Kind:
		return protoreflect.ValueOfInt64(int64(n) + 1)
	default:
		panic(fmt.Sprintf("field %s: no test value for kind %s", fd.FullName(), fd.Kind()))
	}
}

func diff(t *testing.T, what string, got, want proto.Message) {
	t.Helper()
	if !proto.Equal(got, want) {
		t.Errorf("%s did not survive the round trip\n got: %s\nwant: %s", what, prototext.Format(got), prototext.Format(want))
	}
}

// Every field and enum value of a network spec comes back from the kind's
// spec: for a created network through spec.layout, for an imported one
// through the remainder.
func TestNetworkSpecRoundTrips(t *testing.T) {
	for k := range variants {
		want := &cloudv1.NetworkSpec{}
		fill(want.ProtoReflect(), k)
		for _, imported := range []bool{false, true} {
			spec, remainder := convert.NetworkSpecToAPI(want, "production", imported)
			if spec.AccountRef.Name != "production" || (spec.Layout != nil) == imported && want.GetLayout() != nil {
				t.Fatalf("variant %d imported %t: accountRef %q, layout in spec %t", k, imported, spec.AccountRef.Name, spec.Layout != nil)
			}
			if imported != (remainder != nil) && want.GetLayout() != nil {
				t.Errorf("variant %d imported %t: remainder %v", k, imported, remainder)
			}
			diff(t, fmt.Sprintf("network spec %d (imported %t)", k, imported),
				convert.NetworkSpecFromAPI(spec, want.GetBootstrapId(), remainder), want)
		}
	}
}

// Every field and enum value of a cluster spec comes back from the kind's
// spec, with no remainder.
func TestClusterSpecRoundTrips(t *testing.T) {
	for k := range variants {
		want := &cloudv1.ClusterSpec{}
		fill(want.ProtoReflect(), k)
		want.ProtoReflect().Clear(want.ProtoReflect().Descriptor().Fields().ByName("tier"))
		if want.GetNetwork() == nil {
			want.Network = &cloudv1.ClusterNetwork{NetworkId: "n"} // a cluster always has its network
		}
		spec, remainder := convert.ClusterSpecToAPI(want, "shared")
		if spec.Network.NetworkRef.Name != "shared" {
			t.Fatalf("variant %d: networkRef %q", k, spec.Network.NetworkRef.Name)
		}
		if remainder != nil {
			t.Errorf("variant %d: remainder %v", k, remainder)
		}
		diff(t, fmt.Sprintf("cluster spec %d", k), convert.ClusterSpecFromAPI(spec, want.GetNetwork().GetNetworkId(), remainder), want)
	}
}

// What the contract leaves unset stays unset: an optional boolean that is
// false is not one that is absent, and a message that is absent is not an
// empty one.
func TestUnsetStaysUnset(t *testing.T) {
	for name, want := range map[string]*cloudv1.ClusterSpec{
		"nothing optional": {Region: "us-east-1", Network: &cloudv1.ClusterNetwork{NetworkId: "n"}},
		"empty messages": {Network: &cloudv1.ClusterNetwork{NetworkId: "n"}, ControlPlane: &cloudv1.ControlPlaneSpec{
			EndpointAccess: &cloudv1.EndpointAccess{}, SecretsEncryption: &cloudv1.SecretsEncryption{}},
			Access: &cloudv1.AccessSpec{},
			Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{AccessConfig: &cloudv1.AwsAccessConfig{},
				UpgradePolicy:        &cloudv1.AwsUpgradePolicy{},
				ControlPlaneLogGroup: &cloudv1.AwsControlPlaneLogGroup{}}}},
		"false is not absent": {Network: &cloudv1.ClusterNetwork{NetworkId: "n"}, ControlPlane: &cloudv1.ControlPlaneSpec{
			EndpointAccess: &cloudv1.EndpointAccess{EndpointPrivateAccess: ptr.To(false), EndpointPublicAccess: ptr.To(true)}},
			Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Irsa: ptr.To(false),
				ControlPlaneLogGroup: &cloudv1.AwsControlPlaneLogGroup{Enabled: ptr.To(false)}}}},
		"an empty provider is a provider": {Network: &cloudv1.ClusterNetwork{NetworkId: "n"},
			Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}},
	} {
		spec, remainder := convert.ClusterSpecToAPI(want, "shared")
		diff(t, name, convert.ClusterSpecFromAPI(spec, "n", remainder), want)
	}
	flowLogs := &cloudv1.NetworkSpec{Tier: cloudv1.Tier_TIER_0, Region: "us-east-1", FlowLogs: &cloudv1.NetworkFlowLogs{Enabled: ptr.To(false)}}
	spec, remainder := convert.NetworkSpecToAPI(flowLogs, "a", false)
	diff(t, "flow logs turned off", convert.NetworkSpecFromAPI(spec, "", remainder), flowLogs)
}

// The kinds spell the contract's values as documented on their types.
func TestEnumSpellings(t *testing.T) {
	spec, _ := convert.ClusterSpecToAPI(&cloudv1.ClusterSpec{
		ReleaseChannel:    cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA,
		MaintenanceWindow: &cloudv1.MaintenanceWindow{Days: []cloudv1.Weekday{cloudv1.Weekday_WEEKDAY_SATURDAY}},
		Access: &cloudv1.AccessSpec{AccessEntries: []*cloudv1.AccessEntry{{Type: cloudv1.AccessEntryType_ACCESS_ENTRY_TYPE_EC2_LINUX,
			Policies: []*cloudv1.AccessPolicy{{Scope: cloudv1.AccessPolicyScope_ACCESS_POLICY_SCOPE_NAMESPACE}}}}},
		Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{
			AccessConfig:           &cloudv1.AwsAccessConfig{AuthenticationMode: cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_API_AND_CONFIG_MAP},
			EnabledClusterLogTypes: []cloudv1.AwsClusterLogType{cloudv1.AwsClusterLogType_AWS_CLUSTER_LOG_TYPE_CONTROLLER_MANAGER},
		}},
	}, "n")
	layout := convert.LayoutToAPI(&cloudv1.NetworkLayout{NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE,
		Subnets: []*cloudv1.Subnet{{Role: cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE}}})
	for got, want := range map[string]string{
		string(spec.ReleaseChannel):                                          "Beta",
		string(spec.MaintenanceWindow.Days[0]):                               "Saturday",
		string(spec.Access.AccessEntries[0].Type):                            "EC2_LINUX",
		string(spec.Access.AccessEntries[0].Policies[0].Scope):               "Namespace",
		string(spec.AWS.AccessConfig.AuthenticationMode):                     "API_AND_CONFIG_MAP",
		string(spec.AWS.EnabledClusterLogTypes[0]):                           "controllerManager",
		string(layout.NATGatewayMode):                                        "PerZone",
		string(layout.Subnets[0].Role):                                       "Private",
		string(convert.LayoutToAPI(&cloudv1.NetworkLayout{}).NATGatewayMode): "",
	} {
		if got != want {
			t.Errorf("spelled %q, want %q", got, want)
		}
	}
	// A spelling the contract does not have (an edit by hand) is unset,
	// not an error and not another value.
	back := convert.ClusterSpecFromAPI(cloudv1alpha1.ClusterSpec{ReleaseChannel: "Nightly"}, "n", nil)
	if back.GetReleaseChannel() != cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED {
		t.Errorf("unknown spelling became %s", back.GetReleaseChannel())
	}
}

func TestNetworkStatusInto(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC))
	earlier := metav1.NewTime(at.Add(-time.Hour))
	status := cloudv1alpha1.NetworkStatus{
		Inventory:  []cloudv1alpha1.InventoryEntry{{Kind: "VPC", Name: "prod-vpc"}},
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, LastTransitionTime: earlier}},
	}
	convert.NetworkStatusInto(&cloudv1.NetworkStatus{
		State: cloudv1.NetworkState_NETWORK_STATE_UPDATING, Message: "3 of 4 resources ready",
		Conditions: []*cloudv1.Condition{
			{Type: "Ready", Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "ResourcesNotReady", Message: "3 of 4"},
			{Type: "Synced", Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "not a reason!",
				LastTransitionTime: timestamppb.New(earlier.Time)},
			{Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE},
		},
		Resources:  &cloudv1.NetworkResources{CloudNetworkId: "vpc-1", PrivateSubnetIds: []string{"subnet-1", "subnet-1", "subnet-2"}},
		ClusterIds: []string{"prod", "prod"},
		Import: &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_PLANNED, Resources: []*cloudv1.ImportedResource{
			{Kind: "VPC", ExternalName: "vpc-1", Disposition: cloudv1.Disposition_DISPOSITION_WILL_UPDATE, Detail: "tags"},
			{Kind: "Subnet"},
		}},
	}, "NETWORK_STATE_READY", at, &status)

	if status.Phase != cloudv1alpha1.NetworkPhaseUpdating || status.Message != "3 of 4 resources ready" || len(status.Inventory) != 1 {
		t.Errorf("phase %q, message %q, inventory %d", status.Phase, status.Message, len(status.Inventory))
	}
	if len(status.Conditions) != 2 {
		t.Fatalf("%d conditions, want Ready and Synced", len(status.Conditions))
	}
	ready, synced := status.Conditions[0], status.Conditions[1]
	if ready.Reason != "ResourcesNotReady" || !ready.LastTransitionTime.Equal(&earlier) {
		t.Errorf("Ready = %+v, want its reason and the time it already had (its status did not change)", ready)
	}
	if synced.Status != metav1.ConditionTrue || synced.Reason != "Unknown" || !synced.LastTransitionTime.Equal(&earlier) {
		t.Errorf("Synced = %+v, want True, a reason metav1 accepts and the contract's time", synced)
	}
	if r := status.Resources; r.CloudNetworkID != "vpc-1" || len(r.PrivateSubnetIDs) != 2 || len(status.Clusters) != 1 {
		t.Errorf("resources %+v, clusters %v: lists must be sets", r, status.Clusters)
	}
	plan := status.ImportPlan
	if plan.Phase != cloudv1alpha1.ImportPhasePlanned || len(plan.Resources) != 1 || plan.Resources[0].Disposition != cloudv1alpha1.DispositionWillUpdate {
		t.Errorf("import plan = %+v, want the line that has a disposition", plan)
	}

	// Without an observed state the stored one names the phase.
	convert.NetworkStatusInto(&cloudv1.NetworkStatus{}, "NETWORK_STATE_IMPORTED", at, &status)
	if status.Phase != cloudv1alpha1.NetworkPhaseImported || status.ImportPlan != nil || status.Conditions != nil {
		t.Errorf("status without a state: phase %q, plan %v, conditions %v", status.Phase, status.ImportPlan, status.Conditions)
	}
	convert.NetworkStatusInto(nil, "x", at, &status)
	if status.Phase != "" {
		t.Errorf("phase of a state the kind does not have = %q", status.Phase)
	}
}

func TestClusterStatusInto(t *testing.T) {
	var status cloudv1alpha1.ClusterStatus
	convert.ClusterStatusInto(&cloudv1.ClusterStatus{
		State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING, CurrentKubernetesVersion: "1.33", OidcIssuerUrl: "https://oidc",
		Endpoint: &cloudv1.ClusterEndpoint{Url: "https://api", CertificateAuthorityData: []byte("ca")},
		Network:  &cloudv1.NetworkResources{CloudNetworkId: "vpc-1"}, KubeconfigSecret: "prod-kubeconfig",
		Provider: &cloudv1.ClusterStatus_Aws{Aws: &cloudv1.AwsClusterStatus{ClusterArn: "arn", Addons: []*cloudv1.AwsAddonStatus{
			{Name: "coredns", Version: "v1", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY},
			{Name: "coredns"}, {Name: "vpc-cni", Message: "degraded"},
		}}},
	}, "", metav1.Now(), &status)
	if status.Phase != cloudv1alpha1.ClusterPhaseRunning || status.Endpoint.URL != "https://api" || status.Network.CloudNetworkID != "vpc-1" ||
		status.CurrentKubernetesVersion != "1.33" || status.OIDCIssuerURL != "https://oidc" || status.KubeconfigSecret != "prod-kubeconfig" {
		t.Errorf("status = %+v", status)
	}
	if a := status.AWS; a.ClusterARN != "arn" || len(a.Addons) != 2 || !a.Addons[0].Ready || a.Addons[1].Ready || a.Addons[1].Message != "degraded" {
		t.Errorf("aws = %+v, want two add-ons, the first ready", status.AWS)
	}
	if status.Kubernetes != nil {
		t.Errorf("kubernetes = %+v, want unset when the contract has none", status.Kubernetes)
	}

	convert.ClusterStatusInto(&cloudv1.ClusterStatus{Kubernetes: &cloudv1.ClusterKubernetes{Channel: "beta", ChannelMinor: "1.36",
		Available: "1.36", Ending: true, Message: "ending"}}, "", metav1.Now(), &status)
	want := cloudv1alpha1.ClusterKubernetesStatus{Channel: "beta", ChannelMinor: "1.36", Available: "1.36", Ending: true, Message: "ending"}
	if status.Kubernetes == nil || *status.Kubernetes != want {
		t.Errorf("kubernetes = %+v, want %+v", status.Kubernetes, want)
	}
}
