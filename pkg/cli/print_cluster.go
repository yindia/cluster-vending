package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/maintenance"
)

var clusterHeader = []string{"NAME", "ID", "PHASE", "VERSION", "REGION", "AGE"}

// clusterRow is a cluster's row of cluster list; wide adds its network,
// node groups, release and message.
func clusterRow(c *cloudv1.Cluster, wide bool) []string {
	st := c.GetStatus()
	r := []string{orNone(c.GetName()), orNone(c.GetId()), clusterPhase(st), clusterVersion(c), orNone(c.GetSpec().GetRegion()),
		age(c.GetCreateTime())}
	if wide {
		r = append(r, orNone(c.GetSpec().GetNetwork().GetNetworkId()), nodeGroupsColumn(st.GetNodeGroups()),
			orNone(st.GetPlatformRelease()), orNone(oneLine(st.GetMessage())))
	}
	return r
}

func clusterPhase(st *cloudv1.ClusterStatus) string {
	phase := word(st.GetState().String(), "CLUSTER_STATE_")
	if frozen := st.GetFreeze(); frozen != nil && !frozen.GetLetThrough() {
		phase += " (frozen)"
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

// nodeGroupsColumn is the node groups the status reports with their node
// counts: "system:2,platform-pool:3".
func nodeGroupsColumn(groups []*cloudv1.NodeGroupStatus) string {
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		parts = append(parts, fmt.Sprintf("%s:%d", g.GetName(), g.GetNodes()))
	}
	return orNone(strings.Join(parts, ","))
}

func printClusters(w io.Writer, clusters []*cloudv1.Cluster, wide bool) error {
	if len(clusters) == 0 {
		_, err := fmt.Fprintln(w, "No clusters found.")
		return err
	}
	header := clusterHeader
	if wide {
		header = append(append([]string{}, header...), "NETWORK", "NODE GROUPS", "RELEASE", "MESSAGE")
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
	if err := table(w, clusterHeader, [][]string{clusterRow(c, false)}); err != nil {
		return err
	}
	return frozenWrite(w, c.GetStatus().GetFreeze())
}

// versionLine is the cluster's version, with the minor its release
// channel offers when it is on one.
func versionLine(c *cloudv1.Cluster) string {
	v := clusterVersion(c)
	if k := c.GetStatus().GetKubernetes(); k.GetChannelMinor() != "" {
		v += fmt.Sprintf(" (channel %s offers %s)", k.GetChannel(), k.GetChannelMinor())
	}
	return v
}

// kubernetesLines are what the cluster's channel says of its version: the
// next minor it can move to, a minor that is ending (a warning), or that
// it is ahead of its channel.
func kubernetesLines(k *cloudv1.ClusterKubernetes) [][2]string {
	var out [][2]string
	if k.GetAvailable() != "" {
		out = append(out, [2]string{"Available", k.GetAvailable()})
	}
	switch {
	case k.GetEnding():
		out = append(out, [2]string{"WARNING", k.GetMessage()})
	case k.GetAvailable() == "" && k.GetMessage() != "":
		out = append(out, [2]string{"Note", k.GetMessage()})
	}
	return out
}

// releaseLines are how the cluster takes new versions: its release
// channel, its rollout tier and its upgrade mode (unset is stored as
// Manual: it never upgrades by itself).
func releaseLines(c *cloudv1.Cluster) [][2]string {
	s := c.GetSpec()
	mode := "Manual (upgrades only when you change the version)"
	if s.GetUpgradeMode() == cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC {
		mode = "Automatic (follow the channel's minor)"
	}
	return [][2]string{
		{"Channel", word(s.GetReleaseChannel().String(), "RELEASE_CHANNEL_")},
		{"Tier", word(c.GetStatus().GetTier().String(), "CLUSTER_TIER_") + " (from network)"},
		{"Upgrades", mode},
	}
}

// describeCluster is one cluster: its fields, then every resource with
// its state and external name, its conditions, platform components and
// recent events.
func describeCluster(w io.Writer, c *cloudv1.Cluster) error {
	st := c.GetStatus()
	kv := [][2]string{
		{"Name", c.GetName()}, {"ID", c.GetId()}, {"Tenant", c.GetTenantId()}, {"Phase", clusterPhase(st)},
		{"Region", c.GetSpec().GetRegion()}, {"Network", c.GetSpec().GetNetwork().GetNetworkId()},
		{"Version", versionLine(c)},
	}
	kv = append(kv, kubernetesLines(st.GetKubernetes())...)
	kv = append(kv, [][2]string{{"Endpoint", st.GetEndpoint().GetUrl()}, {"Release", releaseLine(st)}}...)
	kv = append(kv, releaseLines(c)...)
	if line := rolloutLine(st.GetRollout(), st.GetCurrentKubernetesVersion()); line != "" {
		kv = append(kv, [2]string{"Rollout", line})
	}
	if line := codeChangeLine(st.GetCodeChange()); line != "" {
		kv = append(kv, [2]string{"Code Change", line})
	}
	if line := catalogChangeLine(st.GetCatalogChange()); line != "" {
		kv = append(kv, [2]string{"Catalog Change", line})
	}
	kv = append(kv, [2]string{"Maintenance", maintenance.Describe(c.GetSpec().GetMaintenanceWindow())})
	if u := st.GetUpgrade(); u != nil {
		kv = append(kv, [2]string{"Upgrade", upgradeLine(u)})
	}
	if p := st.GetFreeze(); p != nil {
		kv = append(kv, [2]string{"Frozen", freezeLine(p)})
	}

	kv = append(kv, summaryLines(st)...)
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
	addons := make([][]string, 0, len(st.GetAws().GetAddons()))
	for _, a := range st.GetAws().GetAddons() {
		addons = append(addons, []string{a.GetName(), orNone(a.GetVersion()), word(a.GetState().String(), "MANAGED_RESOURCE_STATE_"),
			orNone(oneLine(a.GetMessage()))})
	}
	if err := section(w, "Add-ons", []string{"NAME", "VERSION", "STATE", "MESSAGE"}, addons); err != nil {
		return err
	}
	if err := nodeGroupSection(w, st.GetNodeGroups()); err != nil {
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
	if err := recoverySection(w, c.GetId(), st.GetPlatformComponents()); err != nil {
		return err
	}
	return section(w, "Events", eventHeader, eventRows(st.GetRecentEvents()))
}

// nodeGroupSection lists the node groups the controller observed: the
// platform's first, then the user's, with the resource totals of the
// cluster status summary (requests only). Nothing is written for a cluster
// that has reported none yet.
func nodeGroupSection(w io.Writer, groups []*cloudv1.NodeGroupStatus) error {
	if len(groups) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(groups))
	for _, g := range groups {
		owner := "user"
		if g.GetPlatformManaged() {
			owner = "platform"
		}
		ready, cpu, memory, pods := none, none, none, none
		if groupRead(g) {
			ready = strconv.Itoa(int(g.GetReadyNodes()))
			cpu = share(g.GetRequestedCpuMillis(), g.GetAllocatableCpuMillis(), cpuText)
			memory = share(g.GetRequestedMemoryBytes(), g.GetAllocatableMemoryBytes(), memoryText)
			pods = strconv.Itoa(int(g.GetPodCount()))
		}
		rows = append(rows, []string{g.GetName(), owner, word(g.GetPhase().String(), "NODE_GROUP_PHASE_"),
			strconv.Itoa(int(g.GetNodes())), ready, cpu, memory, pods, orNone(oneLine(g.GetMessage()))})
	}
	return section(w, "Node Groups", []string{"NAME", "OWNER", "PHASE", "NODES", "READY", "CPU REQUESTED", "MEMORY REQUESTED", "PODS", "MESSAGE"}, rows)
}
