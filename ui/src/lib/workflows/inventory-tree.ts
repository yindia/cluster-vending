import { CloudAccountState, type CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { NetworkState, type Network } from "../../gen/cloud/v1/network_pb";
import { ClusterState, type Cluster } from "../../gen/cloud/v1/cluster_pb";

export type InventoryNode = {
  id: string; resourceId: string; name: string; kind: string; provider: string;
  state: string; href: string; children: InventoryNode[]; networks: number; clusters: number;
};

function stateLabel(value?: string) {
  if (!value || value.endsWith("UNSPECIFIED")) return "Not reported";
  const text = value.replace(/^(CLOUD_ACCOUNT|NETWORK|CLUSTER)_STATE_/, "").toLowerCase().replaceAll("_", " ");
  return text[0].toUpperCase() + text.slice(1);
}

/** Join immutable platform IDs; missing parents remain explicit, visible inventory. */
export function buildInventoryTree(accounts: readonly CloudAccount[], networks: readonly Network[], clusters: readonly Cluster[]): InventoryNode[] {
  const node = (kind: string, id: string, name: string, provider: string, state: string): InventoryNode => ({
    id: `${kind}:${id}`, resourceId: id, name: name || id, kind, provider, state,
    href: `/${kind.toLowerCase()}s/${encodeURIComponent(id)}`, children: [], networks: 0, clusters: 0,
  });
  const accountNodes = new Map(accounts.map(account => [account.id, node("Account", account.id, account.accountName, account.provider.case === "aws" ? "AWS" : "Unknown", stateLabel(CloudAccountState[account.status?.state ?? 0]))]));
  const networkNodes = new Map(networks.map(network => [network.id, node("Network", network.id, network.displayName || network.name, accountNodes.get(network.spec?.bootstrapId ?? "")?.provider ?? "Unknown", stateLabel(NetworkState[network.status?.state ?? 0]))]));
  const unlinked: InventoryNode = { id: "unlinked", resourceId: "", name: "Unlinked resources", kind: "Group", provider: "", state: "Parent unavailable", href: "", children: [], networks: 0, clusters: 0 };
  for (const network of networks) {
    const parent = accountNodes.get(network.spec?.bootstrapId ?? "") ?? unlinked;
    const child = networkNodes.get(network.id)!;
    if (!parent.children.includes(child)) parent.children.push(child);
  }
  const seen = new Set<string>();
  for (const cluster of clusters) {
    if (seen.has(cluster.id)) continue;
    seen.add(cluster.id);
    const parent = networkNodes.get(cluster.spec?.network?.networkId ?? "") ?? unlinked;
    parent.children.push(node("Cluster", cluster.id, cluster.displayName || cluster.name, cluster.spec?.provider.case === "aws" ? "AWS" : parent.provider || "Unknown", stateLabel(ClusterState[cluster.status?.state ?? 0])));
  }
  function summarize(item: InventoryNode) {
    item.children.sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
    for (const child of item.children) {
      summarize(child);
      item.networks += child.networks + Number(child.kind === "Network");
      item.clusters += child.clusters + Number(child.kind === "Cluster");
    }
  }
  const roots = [...accountNodes.values()].sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id));
  if (unlinked.children.length) roots.push(unlinked);
  roots.forEach(summarize);
  return roots;
}

export type InventoryRow = InventoryNode & { depth: number; context: boolean };
export function filterInventoryTree(roots: readonly InventoryNode[], filters: { search: string; kind: string; state: string; provider: string }, collapsed: ReadonlySet<string>) {
  const terms = filters.search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtering = !!terms.length || [filters.kind, filters.state, filters.provider].some(value => value !== "all");
  const matches = new Set<string>();
  const included = new Set<string>();
  function collect(item: InventoryNode): boolean {
    const text = `${item.name} ${item.resourceId} ${item.kind} ${item.provider} ${item.state}`.toLowerCase();
    const match = item.kind !== "Group" && terms.every(term => text.includes(term)) &&
      (filters.kind === "all" || filters.kind === item.kind) &&
      (filters.state === "all" || filters.state === item.state) &&
      (filters.provider === "all" || filters.provider === item.provider);
    if (match) matches.add(item.id);
    const descendants = item.children.map(collect).some(Boolean);
    if (match || descendants) included.add(item.id);
    return match || descendants;
  }
  roots.forEach(collect);
  const rows: InventoryRow[] = [];
  function visit(item: InventoryNode, depth: number) {
    if (!included.has(item.id)) return;
    rows.push({ ...item, depth, context: !matches.has(item.id) });
    if (filtering || !collapsed.has(item.id)) item.children.forEach(child => visit(child, depth + 1));
  }
  roots.forEach(item => visit(item, 0));
  return { rows, matches: matches.size, filtering };
}
