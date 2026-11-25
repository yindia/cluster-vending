import type { Cluster } from "../../gen/cloud/v1/cluster_pb";
import type { TopologyNodeData } from "./network-topology";

export type ArchitectureDetail = { label: string; value: string; source?: string };
const missing = "Not reported";

/** Node groups and Karpenter pools are created from the platform's configuration; the cluster spec has no settings for them. */
export const PLATFORM_NODE_SOURCE = "Created from the platform's configuration";
export function clusterComputeDetails(cluster: Cluster, node: TopologyNodeData): ArchitectureDetail[] {
  if (node.kind === "NodeGroup" || node.kind === "NodePool") return [
    { label: "Managed by", value: "The platform", source: PLATFORM_NODE_SOURCE },
  ];
  if (["Node", "Instance", "EC2Instance", "WorkerNode", "KarpenterNode"].includes(node.kind)) return [
    { label: "Instance type", value: missing },
    { label: "Availability Zone", value: node.zoneId || missing },
    { label: "Ready status", value: node.state || missing },
    { label: "Kubernetes version", value: missing },
  ];
  if (node.kind === "Cluster") {
    const endpoint = cluster.spec?.controlPlane?.endpointAccess;
    return [
      { label: "Cluster name", value: cluster.name },
      { label: "Cluster ARN", value: cluster.status?.provider.case === "aws" ? cluster.status.provider.value.clusterArn || missing : missing },
      { label: "Kubernetes version", value: cluster.status?.currentKubernetesVersion || missing, source: "Observed control plane" },
      { label: "Public endpoint", value: endpoint ? endpoint.endpointPublicAccess === false ? "Disabled" : "Enabled" : missing, source: "Configured" },
      { label: "Private endpoint", value: endpoint ? endpoint.endpointPrivateAccess === false ? "Disabled" : "Enabled" : missing, source: "Configured" },
      { label: "Public access CIDRs", value: endpoint?.publicAccessCidrs.join(", ") || missing, source: "Configured" },
      { label: "VPC", value: cluster.status?.network?.cloudNetworkId || missing },
    ];
  }
  return [];
}
