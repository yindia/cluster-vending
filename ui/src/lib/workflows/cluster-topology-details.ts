import type { Cluster } from "../../gen/cloud/v1/cluster_pb";
import { CapacityType, type NodePoolSpec } from "../../gen/cloud/v1/node_pool_pb";
import type { TopologyNodeData } from "./network-topology";

export type ArchitectureDetail = { label: string; value: string; source?: string };
const missing = "Not reported";

/** The current API has one default-pool spec and one observed node-group ARN.
 * Never apply that spec to every group, or guess a match from a resource suffix.
 * Multiple groups make the old singular status ambiguous, so retain unknowns. */
export function configuredNodePool(cluster: Cluster, node: TopologyNodeData): NodePoolSpec | undefined {
  if (node.kind !== "NodeGroup" || node.referenceOnly || !cluster.spec?.defaultNodePool) return;
  const groups = cluster.status?.architecture?.resources.filter(resource => resource.kind === "NodeGroup" && !resource.referenceOnly) ?? [];
  if (groups.length !== 1 || groups[0].name !== node.resourceId) return;
  const aws = cluster.status?.provider.case === "aws" ? cluster.status.provider.value : undefined;
  const arn = aws?.nodeGroupArn;
  if (!arn) return;
  const match = /^arn:[^:]+:eks:[^:]+:\d{12}:nodegroup\/([^/]+)\/([^/]+)\/[^/]+$/.exec(arn);
  if (!match || ![arn, match[2], `${match[1]}:${match[2]}`].includes(node.cloudId)) return;
  return cluster.spec.defaultNodePool;
}
export function clusterComputeDetails(cluster: Cluster, node: TopologyNodeData): ArchitectureDetail[] {
  if (node.kind === "NodeGroup" || node.kind === "NodePool") {
    const pool = configuredNodePool(cluster, node);
    const source = pool ? "Configured default pool" : undefined;
    return [
      { label: "Instance types", value: pool?.instanceTypes.join(", ") || missing, source },
      { label: "Capacity", value: pool ? pool.capacityType === CapacityType.SPOT ? "Spot" : pool.capacityType === CapacityType.ON_DEMAND ? "On-Demand" : "On-Demand (default)" : missing, source },
      { label: "Desired nodes", value: missing },
      { label: "Minimum nodes", value: pool?.scalingConfig ? String(pool.scalingConfig.minSize) : missing, source },
      { label: "Maximum nodes", value: pool?.scalingConfig ? String(pool.scalingConfig.maxSize) : missing, source },
      { label: "Current nodes", value: missing },
      ...(pool?.scalingConfig?.desiredSize !== undefined ? [{ label: "Initial desired nodes", value: String(pool.scalingConfig.desiredSize), source: "Create-time setting; not the current autoscaler target" }] : []),
    ];
  }
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
