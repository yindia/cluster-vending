package cli

import (
	"fmt"
	"io"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var clusterHeader = []string{"NAME", "ID", "PHASE", "VERSION", "REGION", "AGE"}

// clusterRow is a cluster's row of cluster list; wide adds its network,
// node pool, release and message.
func clusterRow(c *cloudv1.Cluster, wide bool) []string {
	st := c.GetStatus()
	r := []string{orNone(c.GetName()), orNone(c.GetId()), clusterPhase(st), clusterVersion(c), orNone(c.GetSpec().GetRegion()),
		age(c.GetCreateTime())}
	if wide {
		r = append(r, orNone(c.GetSpec().GetNetwork().GetNetworkId()), nodePool(c.GetSpec().GetDefaultNodePool()),
			orNone(st.GetPlatformRelease()), orNone(oneLine(st.GetMessage())))
	}
	return r
}

func clusterPhase(st *cloudv1.ClusterStatus) string {
	phase := word(st.GetState().String(), "CLUSTER_STATE_")
	if st.GetPause() != nil {
		phase += " (paused)"
	}
	return phase
}

// clusterVersion is the Kubernetes version the cluster runs, else the one
// it asks for.
func clusterVersion(c *cloudv1.Cluster) string {
	if v := c.GetStatus().GetCurrentKubernetesVersion(); v != "" {
		return v
	}
	return orNone(c.GetSpec().GetControlPlane().GetKubernetesVersion())
}

// nodePool is a node pool's instance types and size range.
func nodePool(p *cloudv1.NodePoolSpec) string {
	if p == nil {
		return none
	}
	types := orNone(strings.Join(p.GetInstanceTypes(), ","))
	if sc := p.GetScalingConfig(); sc != nil {
		return fmt.Sprintf("%s (%d-%d)", types, sc.GetMinSize(), sc.GetMaxSize())
	}
	return types
}

func printClusters(w io.Writer, clusters []*cloudv1.Cluster, wide bool) error {
	if len(clusters) == 0 {
		_, err := fmt.Fprintln(w, "No clusters found.")
		return err
	}
	header := clusterHeader
	if wide {
		header = append(append([]string{}, header...), "NETWORK", "NODE POOL", "RELEASE", "MESSAGE")
	}
	rows := make([][]string, 0, len(clusters))
	for _, c := range clusters {
		rows = append(rows, clusterRow(c, wide))
	}
	return table(w, header, rows)
}

// mutatedCluster is what a cluster call changed: kubectl's line, then the
// cluster's row.
func mutatedCluster(w io.Writer, c *cloudv1.Cluster, verb string) error {
	if err := mutated(w, "cluster", c.GetName(), c.GetId(), verb); err != nil {
		return err
	}
	return table(w, clusterHeader, [][]string{clusterRow(c, false)})
}

// describeCluster is one cluster: its fields, then every resource with
// its state and external name, its conditions, platform components and
// recent events.
func describeCluster(w io.Writer, c *cloudv1.Cluster) error {
	st := c.GetStatus()
	kv := [][2]string{
		{"Name", c.GetName()}, {"ID", c.GetId()}, {"Tenant", c.GetTenantId()}, {"Phase", clusterPhase(st)},
		{"Region", c.GetSpec().GetRegion()}, {"Network", c.GetSpec().GetNetwork().GetNetworkId()},
		{"Version", clusterVersion(c)}, {"Endpoint", st.GetEndpoint().GetUrl()}, {"Release", st.GetPlatformRelease()},
	}
	if u := st.GetUpgrade(); u != nil {
		kv = append(kv, [2]string{"Upgrade", fmt.Sprintf("%s to %s (%s)", orNone(u.GetFromVersion()), orNone(u.GetToVersion()),
			word(u.GetPhase().String(), "CLUSTER_UPGRADE_PHASE_"))})
	}
	if p := st.GetPause(); p != nil {
		kv = append(kv, [2]string{"Paused", pauseLine(p)})
	}
	kv = append(kv, [2]string{"Message", oneLine(st.GetMessage())}, [2]string{"Age", age(c.GetCreateTime())})
	if err := fields(w, kv); err != nil {
		return err
	}
	if err := section(w, "Resources", resourceHeader, resourceRows(st.GetManagedResources())); err != nil {
		return err
	}
	if err := section(w, "Conditions", conditionHeader, conditionRows(st.GetConditions())); err != nil {
		return err
	}
	components := make([][]string, 0, len(st.GetPlatformComponents()))
	for _, p := range st.GetPlatformComponents() {
		components = append(components, []string{p.GetName(), orNone(p.GetVersion()), word(p.GetPhase().String(), "COMPONENT_PHASE_"),
			age(p.GetSince()), orNone(oneLine(p.GetMessage()))})
	}
	if err := section(w, "Platform Components", []string{"NAME", "VERSION", "PHASE", "AGE", "MESSAGE"}, components); err != nil {
		return err
	}
	return section(w, "Events", eventHeader, eventRows(st.GetRecentEvents()))
}
