package cli

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var networkHeader = []string{"NAME", "ID", "PHASE", "REGION", "CLUSTERS", "AGE"}

// networkRow is a network's row of network list; wide adds its VPC, CIDR,
// zones and NAT gateway mode.
func networkRow(n *cloudv1.Network, wide bool) []string {
	st := n.GetStatus()
	r := []string{orNone(n.GetName()), orNone(n.GetId()), networkPhase(st), orNone(n.GetSpec().GetRegion()),
		strconv.Itoa(len(st.GetClusterIds())), age(n.GetCreateTime())}
	if wide {
		layout := n.GetSpec().GetLayout()
		r = append(r, orNone(st.GetResources().GetCloudNetworkId()), orNone(layout.GetCidrBlock()), orNone(strings.Join(zones(layout), ",")),
			word(layout.GetNatGatewayMode().String(), "NAT_GATEWAY_MODE_"))
	}
	return r
}

func networkPhase(st *cloudv1.NetworkStatus) string {
	phase := word(st.GetState().String(), "NETWORK_STATE_")
	if st.GetPause() != nil {
		phase += " (paused)"
	}
	return phase
}

// zones are the zones of a layout's subnets, each once, in order.
func zones(layout *cloudv1.NetworkLayout) []string {
	var out []string
	for _, s := range layout.GetSubnets() {
		if z := s.GetZoneId(); z != "" && !slices.Contains(out, z) {
			out = append(out, z)
		}
	}
	return out
}

func printNetworks(w io.Writer, networks []*cloudv1.Network, wide bool) error {
	if len(networks) == 0 {
		_, err := fmt.Fprintln(w, "No networks found.")
		return err
	}
	header := networkHeader
	if wide {
		header = append(append([]string{}, header...), "VPC", "CIDR", "ZONES", "NAT")
	}
	rows := make([][]string, 0, len(networks))
	for _, n := range networks {
		rows = append(rows, networkRow(n, wide))
	}
	return table(w, header, rows)
}

func mutatedNetwork(w io.Writer, n *cloudv1.Network, verb string) error {
	if err := mutated(w, "network", n.GetName(), n.GetId(), verb); err != nil {
		return err
	}
	return table(w, networkHeader, [][]string{networkRow(n, false)})
}

// describeNetwork is one network: its fields, then every resource with
// its state and external name, its conditions and recent events.
func describeNetwork(w io.Writer, n *cloudv1.Network) error {
	st := n.GetStatus()
	layout := n.GetSpec().GetLayout()
	kv := [][2]string{
		{"Name", n.GetName()}, {"ID", n.GetId()}, {"Tenant", n.GetTenantId()}, {"Phase", networkPhase(st)},
		{"Region", n.GetSpec().GetRegion()}, {"VPC", st.GetResources().GetCloudNetworkId()}, {"CIDR", layout.GetCidrBlock()},
		{"Zones", strings.Join(zones(layout), ",")}, {"Clusters", strings.Join(st.GetClusterIds(), ",")},
	}
	if p := st.GetPause(); p != nil {
		kv = append(kv, [2]string{"Paused", pauseLine(p)})
	}
	kv = append(kv, [2]string{"Message", oneLine(st.GetMessage())}, [2]string{"Age", age(n.GetCreateTime())})
	if err := fields(w, kv); err != nil {
		return err
	}
	if err := section(w, "Resources", resourceHeader, resourceRows(st.GetManagedResources())); err != nil {
		return err
	}
	if err := section(w, "Conditions", conditionHeader, conditionRows(st.GetConditions())); err != nil {
		return err
	}
	return section(w, "Events", eventHeader, eventRows(st.GetRecentEvents()))
}
