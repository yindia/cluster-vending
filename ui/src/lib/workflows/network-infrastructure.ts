import type { Network, NetworkArchitectureConnection, NetworkArchitectureResource } from "../../gen/cloud/v1/network_pb";

export const resourceKey = (kind: string, name: string) => JSON.stringify([kind, name]);
export type NetworkInfrastructure = {
  networkId: string; region: string;
  resources: ReadonlyMap<string, NetworkArchitectureResource>;
  connections: readonly NetworkArchitectureConnection[];
  identities: ReadonlyMap<string, string>;
  diagnostics: string[];
};
/** Read-only provider facts. No topology is inferred from names, desired counts,
 * AZ proximity, or inventory ownership. Cloud IDs are canonical when available. */
export function readNetworkInfrastructure(network?: Network): NetworkInfrastructure {
  const resources = new Map<string, NetworkArchitectureResource>();
  const identities = new Map<string, string>();
  const diagnostics: string[] = [];
  const scope = network?.id ?? "";
  for (const fact of [...network?.status?.architecture?.resources ?? []].sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)))) {
    if (fact.kind && fact.name) resources.set(resourceKey(fact.kind, fact.name), fact);
  }
  const cloudCounts = new Map<string, number>();
  for (const fact of resources.values()) if (fact.cloudId) {
    const key = resourceKey(fact.kind, fact.cloudId);
    cloudCounts.set(key, (cloudCounts.get(key) ?? 0) + 1);
  }
  for (const [key, fact] of resources) {
    const cloud = fact.cloudId && cloudCounts.get(resourceKey(fact.kind, fact.cloudId)) === 1;
    identities.set(key, `architecture:${encodeURIComponent(scope)}:${encodeURIComponent(fact.kind)}:${cloud ? "aws" : "managed"}:${encodeURIComponent(cloud ? fact.cloudId : fact.name)}`);
    if (fact.cloudId && !cloud) diagnostics.push(`${fact.kind}: cloud identity ${fact.cloudId} is ambiguous; managed object identities are retained.`);
  }
  const edges = new Map<string, NetworkArchitectureConnection>();
  for (const edge of network?.status?.architecture?.connections ?? []) {
    if (edge.evidence !== "configured" && edge.evidence !== "observed") continue;
    if (!resources.has(resourceKey(edge.sourceKind, edge.sourceName)) || !resources.has(resourceKey(edge.targetKind, edge.targetName))) {
      diagnostics.push("A network reference has an unavailable endpoint; its connection is not drawn."); continue;
    }
    const key = JSON.stringify([edge.sourceKind, edge.sourceName, edge.targetKind, edge.targetName, edge.relation, edge.label]);
    if (!edges.has(key) || edge.evidence === "observed") edges.set(key, edge);
  }
  if (scope && !network?.status?.architecture) diagnostics.push("Architecture references are not available; inventory alone does not prove traffic paths.");
  return { networkId: scope, region: network?.spec?.region ?? "", resources, identities,
    connections: [...edges.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([, edge]) => edge), diagnostics };
}
