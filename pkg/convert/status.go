package convert

import (
	"regexp"
	"slices"

	"google.golang.org/protobuf/reflect/protoreflect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// maxConditionMessage is the longest message a metav1.Condition may have.
const maxConditionMessage = 32768

// reasonPattern is what a metav1.Condition accepts as its reason.
var reasonPattern = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

// NetworkStatusInto writes what a contract status says into the kind's
// status: phase, message, conditions, resources, clusters and the import
// plan. The fields the contract's status has no say in (inventory, layout,
// observedGeneration, contractStatus) are left as they are. state is the
// phase's source when the status carries none (the stored lifecycle
// state); at is the transition time of conditions that changed.
func NetworkStatusInto(p *cloudv1.NetworkStatus, state string, at metav1.Time, into *cloudv1alpha1.NetworkStatus) {
	into.Phase = cloudv1alpha1.NetworkPhase(phase(p.GetState(), prefixNetworkState, state))
	into.Message = p.GetMessage()
	into.Conditions = conditions(p.GetConditions(), into.Conditions, at)
	into.Resources = resources(p.GetResources())
	into.Clusters = unique(p.GetClusterIds())
	into.ImportPlan = importPlan(p.GetImport())
}

// ClusterStatusInto is NetworkStatusInto for a cluster: phase, message,
// conditions, endpoint, versions, issuer, network resources, the EKS
// cluster's state, the import plan and the kubeconfig Secret.
func ClusterStatusInto(p *cloudv1.ClusterStatus, state string, at metav1.Time, into *cloudv1alpha1.ClusterStatus) {
	into.Phase = cloudv1alpha1.ClusterPhase(phase(p.GetState(), prefixClusterState, state))
	into.Message = p.GetMessage()
	into.Conditions = conditions(p.GetConditions(), into.Conditions, at)
	into.Endpoint = nil
	if e := p.GetEndpoint(); e != nil {
		into.Endpoint = &cloudv1alpha1.ClusterEndpoint{URL: e.GetUrl(), CertificateAuthorityData: e.GetCertificateAuthorityData()}
	}
	into.CurrentKubernetesVersion = p.GetCurrentKubernetesVersion()
	into.OIDCIssuerURL = p.GetOidcIssuerUrl()
	into.Network = resources(p.GetNetwork())
	into.AWS = awsStatus(p.GetAws())
	into.ImportPlan = importPlan(p.GetImport())
	into.KubeconfigSecret = p.GetKubeconfigSecret()
	into.Upgrade = upgrade(p.GetUpgrade())
}

// upgradePhases are the kind's spellings of the contract's upgrade phases.
var upgradePhases = map[cloudv1.ClusterUpgradePhase]cloudv1alpha1.ClusterUpgradePhase{
	cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE:    cloudv1alpha1.ClusterUpgradeControlPlane,
	cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS: cloudv1alpha1.ClusterUpgradeNodesAndAddons,
}

// upgrade is the kind's view of a Kubernetes upgrade in progress; nil
// when there is none.
func upgrade(p *cloudv1.ClusterUpgrade) *cloudv1alpha1.ClusterUpgrade {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.ClusterUpgrade{FromVersion: p.GetFromVersion(), ToVersion: p.GetToVersion(),
		Phase: upgradePhases[p.GetPhase()], Message: p.GetMessage()}
	if p.GetStartTime() != nil {
		out.StartTime = &metav1.Time{Time: p.GetStartTime().AsTime()}
	}
	return out
}

// phase is the kind's phase of a contract state; of the stored state name
// (e.g. "NETWORK_STATE_READY") when the status has none. "" when neither
// names a state the kind has.
func phase(observed protoreflect.Enum, prefix, stored string) string {
	if p := camel(observed, prefix); p != "" {
		return p
	}
	if observed.Descriptor().Values().ByName(protoreflect.Name(stored)) == nil {
		return ""
	}
	p := camelName(stored, prefix)
	if p == "Unspecified" {
		return ""
	}
	return p
}

// conditions are the contract's conditions as metav1.Conditions. One whose
// status did not change keeps the transition time it has in existing; the
// others get the contract's, or at. A condition metav1 cannot hold (no
// type) is left out; a reason it would refuse becomes "Unknown".
func conditions(in []*cloudv1.Condition, existing []metav1.Condition, at metav1.Time) []metav1.Condition {
	var out []metav1.Condition
	for _, c := range in {
		if c.GetType() == "" || slices.ContainsFunc(out, func(o metav1.Condition) bool { return o.Type == c.GetType() }) {
			continue
		}
		cond := metav1.Condition{Type: c.GetType(), Status: conditionStatus(c.GetStatus()), Reason: c.GetReason(),
			Message: c.GetMessage(), LastTransitionTime: at}
		if !reasonPattern.MatchString(cond.Reason) {
			cond.Reason = "Unknown"
		}
		if len(cond.Message) > maxConditionMessage {
			cond.Message = cond.Message[:maxConditionMessage]
		}
		if t := c.GetLastTransitionTime(); t != nil {
			cond.LastTransitionTime = metav1.NewTime(t.AsTime())
		} else if i := slices.IndexFunc(existing, func(o metav1.Condition) bool { return o.Type == cond.Type }); i >= 0 &&
			existing[i].Status == cond.Status {
			cond.LastTransitionTime = existing[i].LastTransitionTime
		}
		out = append(out, cond)
	}
	return out
}

func conditionStatus(s cloudv1.ConditionStatus) metav1.ConditionStatus {
	switch s {
	case cloudv1.ConditionStatus_CONDITION_STATUS_TRUE:
		return metav1.ConditionTrue
	case cloudv1.ConditionStatus_CONDITION_STATUS_FALSE:
		return metav1.ConditionFalse
	default:
		return metav1.ConditionUnknown
	}
}

// resources are a network's cloud identifiers; each list without
// duplicates (the kind's lists are sets).
func resources(p *cloudv1.NetworkResources) *cloudv1alpha1.NetworkResources {
	if p == nil {
		return nil
	}
	return &cloudv1alpha1.NetworkResources{
		CloudNetworkID:   p.GetCloudNetworkId(),
		PublicSubnetIDs:  unique(p.GetPublicSubnetIds()),
		PrivateSubnetIDs: unique(p.GetPrivateSubnetIds()),
		NATGatewayIDs:    unique(p.GetNatGatewayIds()),
		EgressIPs:        unique(p.GetEgressIps()),
	}
}

// unique is in without repeated values, in its order.
func unique(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// importPlan is the contract's import status as the kinds' plan; nil for
// none. A line without kind or disposition (which the kind requires) is
// left out: the contract status kept next to it has every line.
func importPlan(p *cloudv1.ImportStatus) *cloudv1alpha1.ImportPlan {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.ImportPlan{Phase: cloudv1alpha1.ImportPhase(camel(p.GetPhase(), prefixImportPhase))}
	for _, r := range p.GetResources() {
		disposition := camel(r.GetDisposition(), prefixDisposition)
		if r.GetKind() == "" || disposition == "" {
			continue
		}
		out.Resources = append(out.Resources, cloudv1alpha1.ImportedResource{
			APIVersion: r.GetApiVersion(), Kind: r.GetKind(), Name: r.GetName(), ExternalName: r.GetExternalName(),
			SpecPath: r.GetSpecPath(), Disposition: cloudv1alpha1.Disposition(disposition), Detail: r.GetDetail(),
			RequiresReplacement: r.GetRequiresReplacement(),
		})
	}
	return out
}

// awsStatus is the observed EKS cluster; nil for none. Add-ons are keyed
// by name in the kind: a second entry of a name is left out.
func awsStatus(p *cloudv1.AwsClusterStatus) *cloudv1alpha1.AWSClusterStatus {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.AWSClusterStatus{ClusterARN: p.GetClusterArn(), ClusterSecurityGroupID: p.GetClusterSecurityGroupId(),
		PlatformVersion: p.GetPlatformVersion(), NodeGroupARN: p.GetNodeGroupArn()}
	for _, a := range p.GetAddons() {
		if a.GetName() == "" || slices.ContainsFunc(out.Addons, func(o cloudv1alpha1.AWSAddonStatus) bool { return o.Name == a.GetName() }) {
			continue
		}
		out.Addons = append(out.Addons, cloudv1alpha1.AWSAddonStatus{Name: a.GetName(), Version: a.GetVersion(),
			Ready:   a.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY,
			Message: a.GetMessage()})
	}
	return out
}
