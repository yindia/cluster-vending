import type { Cluster } from "../../gen/cloud/v1/cluster_pb";
import { ClusterState } from "../../gen/cloud/v1/cluster_pb";
import { ManagedResourceState } from "../../gen/cloud/v1/managed_resource_pb";
import { supportsCapability } from "../providers/registry";

export type ClusterAccessCommand =
  | { state: "ready"; provider: "aws"; command: string; region: string; clusterName: string; accountId?: string }
  | { state: "pending" | "unsupported" | "invalid"; message: string };

/** POSIX shell arguments are data, including quotes and command substitutions. */
export function shellArgument(value: string): string {
  if (value.includes("\0")) throw new Error("Shell arguments cannot contain NUL.");
  return `'${value.replaceAll("'", "'\\''")}'`;
}

const eksName = /^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$/;
const awsRegion = /^[a-z]{2}(?:-gov|-iso[a-z]?)?-[a-z]+-[0-9]+$/;
const eksArn = /^arn:(aws|aws-cn|aws-us-gov|aws-iso|aws-iso-b|aws-iso-e|aws-iso-f):eks:([a-z0-9-]+):([0-9]{12}):cluster\/([A-Za-z0-9][A-Za-z0-9_-]{0,99})$/;

function regionPartition(region: string): string {
  if (region.startsWith("cn-")) return "aws-cn";
  if (region.startsWith("us-gov-")) return "aws-us-gov";
  if (region.startsWith("us-iso-")) return "aws-iso";
  if (region.startsWith("us-isob-")) return "aws-iso-b";
  if (region.startsWith("eu-isoe-")) return "aws-iso-e";
  if (region.startsWith("us-isof-")) return "aws-iso-f";
  return "aws";
}

function parseEksArn(value: string, region: string) {
  const match = eksArn.exec(value);
  if (!match || match[0] !== value || match[2] !== region || match[1] !== regionPartition(region)) return undefined;
  return { clusterName: match[4], accountId: match[3] };
}

// Only this adapter understands EKS identifiers. Shared access UI never guesses
// cloud names from the platform's logical Cluster.name or inventory object name.
function awsAccessCommand(cluster: Cluster): ClusterAccessCommand {
  const region = cluster.spec?.region;
  if (!region) return { state: "pending", message: "The cluster region has not been reported yet. Refresh after the control plane is discovered." };
  if (awsRegion.exec(region)?.[0] !== region) return { state: "invalid", message: "The reported region is not a valid AWS region. No command can be generated safely." };
  if (cluster.status?.state === ClusterState.DELETING) return { state: "pending", message: "This cluster is being deleted. Kubeconfig setup is unavailable." };
  if (cluster.status?.provider.case && cluster.status.provider.case !== "aws") return { state: "invalid", message: "The reported cluster provider does not match AWS." };

  const arn = cluster.status?.provider.case === "aws" ? cluster.status.provider.value.clusterArn : "";
  const fromArn = arn ? parseEksArn(arn, region) : undefined;
  if (arn && !fromArn) return { state: "invalid", message: "The reported EKS ARN does not match the AWS region or has an invalid cluster identity. No command was generated." };

  const resources = cluster.status?.managedResources.filter((resource) => resource.kind === "Cluster") ?? [];
  const observed = resources.filter((resource) => resource.externalName && !resource.deletionTime && resource.state !== ManagedResourceState.MISSING && resource.state !== ManagedResourceState.DELETING);
  const identities: { clusterName: string; accountId?: string }[] = [];
  for (const resource of observed) {
    const identity = resource.externalName.startsWith("arn:")
      ? parseEksArn(resource.externalName, region)
      : eksName.exec(resource.externalName)?.[0] === resource.externalName ? { clusterName: resource.externalName } : undefined;
    if (!identity) return { state: "invalid", message: "The managed EKS resource has an invalid external identity. No command was generated." };
    identities.push(identity);
  }
  if (fromArn) identities.unshift(fromArn);
  const identity = identities.find((item) => item.accountId) ?? identities[0];
  if (!identity || (resources.length > 0 && observed.length === 0)) return { state: "pending", message: "The physical EKS cluster name is not available yet. Refresh after the control plane is discovered; the platform name is not used as a substitute." };
  if (identities.some((item) => item.clusterName !== identity.clusterName || (item.accountId && identity.accountId && item.accountId !== identity.accountId))) {
    return { state: "invalid", message: "The reported EKS identities disagree. Refresh cluster details before copying a command." };
  }
  return {
    state: "ready", provider: "aws", region, ...identity,
    command: `aws eks update-kubeconfig --region ${shellArgument(region)} --name ${shellArgument(identity.clusterName)}`,
  };
}

/** Accept only a fresh response bound to the exact requested tenant and cluster. */
export function clusterAccessCommand(cluster: Cluster, tenantId: string, clusterId: string): ClusterAccessCommand {
  if (!tenantId || !clusterId || cluster.tenantId !== tenantId || cluster.id !== clusterId) {
    return { state: "invalid", message: "Cluster details do not match the selected tenant and cluster. Reopen this dialog after refreshing the page." };
  }
  const provider = cluster.spec?.provider.case ?? "";
  if (!supportsCapability(provider, "cluster.kubeconfig")) return { state: "unsupported", message: "Kubeconfig commands are not supported for this provider." };
  if (provider === "aws") return awsAccessCommand(cluster);
  return { state: "unsupported", message: "No kubeconfig command adapter is available for this provider." };
}
