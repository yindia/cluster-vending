package clustersummary

import (
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// NodeRow is the row of a node: only what the console shows. The
// controller's node pass and the live reader both project nodes with it.
func NodeRow(n *unstructured.Unstructured) *cloudv1.NodeGroupNode {
	labels := n.GetLabels()
	row := &cloudv1.NodeGroupNode{
		Name: n.GetName(), InstanceType: labels["node.kubernetes.io/instance-type"], Zone: labels["topology.kubernetes.io/zone"],
		CapacityType: capacityTypeOf(labels), Architecture: labels["kubernetes.io/arch"],
	}
	row.KubeletVersion, _, _ = unstructured.NestedString(n.Object, "status", "nodeInfo", "kubeletVersion")
	row.ProviderId, _, _ = unstructured.NestedString(n.Object, "spec", "providerID")
	row.Unschedulable, _, _ = unstructured.NestedBool(n.Object, "spec", "unschedulable")
	taints, _, _ := unstructured.NestedSlice(n.Object, "spec", "taints")
	row.TaintsCount = int32(len(taints)) //nolint:gosec // a node has few taints
	if t := n.GetCreationTimestamp(); !t.IsZero() {
		row.CreatedAt = timestamppb.New(t.UTC())
	}
	conds, _, _ := unstructured.NestedSlice(n.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := c["type"].(string)
		on := c["status"] == "True"
		switch typ {
		case "Ready":
			row.Ready = on
		case "MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable":
			if on {
				row.Conditions = append(row.Conditions, typ)
			}
		}
	}
	return row
}

// capacityTypeOf is "on-demand" or "spot": Karpenter's label, else the
// managed node group's (ON_DEMAND, SPOT).
func capacityTypeOf(labels map[string]string) string {
	if v := labels["karpenter.sh/capacity-type"]; v != "" {
		return v
	}
	return strings.ReplaceAll(strings.ToLower(labels["eks.amazonaws.com/capacityType"]), "_", "-")
}

// ClaimInto puts the NodeClaim's name, phase and Ready condition on the row.
func ClaimInto(row *cloudv1.NodeGroupNode, claim *unstructured.Unstructured) {
	row.NodeclaimName = claim.GetName()
	conds, _, _ := unstructured.NestedSlice(claim.Object, "status", "conditions")
	on := map[string]bool{}
	for _, raw := range conds {
		if c, ok := raw.(map[string]any); ok {
			typ, _ := c["type"].(string)
			on[typ] = c["status"] == "True"
		}
	}
	row.NodeclaimPhase = "Pending"
	for _, phase := range []string{"Initialized", "Registered", "Launched"} {
		if on[phase] {
			row.NodeclaimPhase = phase
			break
		}
	}
	row.NodeclaimReady = on["Ready"]
	if row.ProviderId == "" {
		row.ProviderId, _, _ = unstructured.NestedString(claim.Object, "status", "providerID")
	}
}
