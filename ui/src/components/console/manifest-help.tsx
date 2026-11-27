"use client";

import { useCallback } from "react";
import { Heading } from "@astryxdesign/core/Heading";
import { Link } from "@astryxdesign/core/Link";
import { List, ListItem } from "@astryxdesign/core/List";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery } from "@/lib/api/use-query";
import type { ManifestKind } from "@/lib/workflows/manifest-yaml";
import { CopyValue } from "./copy-value";

export type ManifestMode = "create" | "import" | "update";

const examples: Record<ManifestKind, string> = {
  CloudAccount: "cloudaccount.yaml", Network: "network.yaml", Cluster: "cluster.yaml", NetworkImport: "network-import.yaml", ClusterImport: "cluster-import.yaml",
};
const nouns: Record<ManifestKind, string> = { CloudAccount: "account", Network: "network", Cluster: "cluster", NetworkImport: "network", ClusterImport: "cluster" };

function command(kind: ManifestKind, mode: ManifestMode) {
  const verb = mode === "update" ? "update --id <ID>" : mode === "import" ? "import" : "create";
  return `cluster-vending ${nouns[kind]} ${verb} -f ${examples[kind]}`;
}

/** The rules review enforces for a kind, said where the user writes the manifest. */
function rules(kind: ManifestKind, mode: ManifestMode): { label: string; description: string }[] {
  const update = mode === "update";
  const common = update
    ? [{ label: "Fields you leave out stay as they are", description: "Only fields whose value differs from the object now are sent, in an update mask with its etag." }]
    : [{ label: "Replace every placeholder ID", description: "Review refuses 00000000-0000-0000-0000-000000000000. References below lists real IDs." }];
  switch (kind) {
    case "Cluster": return [...common,
      { label: "Add your own administrator", description: "Only the platform's role is admin by default. Add your IAM role or user ARN to spec.access.adminPrincipals, or no one else can use kubectl." },
      { label: "Extended support", description: "A minor the release offers on no channel runs with UPGRADE_MODE_MANUAL and AWS_SUPPORT_TYPE_EXTENDED only." },
      { label: "Node groups are the platform's", description: "nodeGroups, defaultNodePool and naas are refused: every cluster gets the system node group and Karpenter's platform pool." },
      { label: "Tier comes from the network", description: "Do not set spec.tier." },
      ...(update ? [{ label: "Fixed after creation", description: "metadata.name, spec.region, spec.network and create-time fields such as zoneIds cannot change." }] : []),
    ];
    case "Network": return [...common,
      { label: "Clusters inherit the tier", description: "TIER_2 rolls out first, TIER_0 last (default)." },
      ...(update
        ? [{ label: "Fixed after creation", description: "metadata.name, spec.bootstrapId, spec.region and spec.layout cannot change." }, { label: "Flow logs", description: "trafficType and aggregation cannot change while enabled: disable, apply, then enable again." }]
        : [{ label: "Layout is immutable", description: "Omit layout for 10.0.0.0/16, 3 zones and one NAT gateway." }]),
    ];
    case "CloudAccount": return [...common,
      { label: update ? "The AWS account ID is fixed" : "Connects an existing account", description: update ? "Change the account name or the role; connect another account for another ID." : "Reusing an AWS account ID updates its existing connection. No AWS account is created." },
      { label: "Role name only", description: "spec.aws.role is the role name, without an ARN or path. The Access tab gives its CloudFormation template." },
    ];
    default: return [...common,
      { label: "Read-only discovery", description: "Import records what exists and changes nothing. Review the discovered plan, then take over explicitly." },
      { label: "No labels on import", description: "Set metadata.labels after the takeover." },
    ];
  }
}

/** The tenant's accounts or networks a manifest refers to, with copyable IDs. */
function References({ kind }: { kind: ManifestKind }) {
  const { api, tenantId } = usePlatform();
  const accounts = kind === "Network" || kind === "NetworkImport";
  const load = useCallback(async (signal: AbortSignal) => {
    if (accounts) return (await api.tenant.listCloudAccounts({ tenantId }, { signal })).accounts.map((item) => ({ id: item.id, name: item.accountName || item.id, detail: item.provider.case === "aws" ? `AWS ${item.provider.value.accountId}` : "Unsupported provider" }));
    return (await api.network.listNetworks({ tenantId, pageSize: 200 }, { signal })).networks.map((item) => ({ id: item.id, name: item.displayName || item.name, detail: item.spec?.region || "Region pending" }));
  }, [api, tenantId, accounts]);
  const query = useAsyncQuery(load);
  const field = accounts ? "spec.bootstrapId" : kind === "Cluster" ? "spec.network.networkId" : "spec.networkId";
  return <VStack gap={2} as="section" aria-labelledby="manifest-references">
    <Heading level={3} accessibilityLevel={4} id="manifest-references">References</Heading>
    <Text type="supporting">{accounts ? "Cloud accounts" : "Networks"} of this tenant, for {field}.</Text>
    {query.loading && <Text role="status" type="supporting">Loading…</Text>}
    {query.error && <Text type="supporting" role="alert">Could not load them: {query.error}</Text>}
    {query.data && !query.data.length && <Text type="supporting">None yet. {accounts ? "Connect a cloud account first." : "Create or import a network first."}</Text>}
    {query.data?.map((item) => <VStack key={item.id} gap={0} data-manifest-reference="">
      <Text type="label" maxLines={1}>{item.name} <Text type="supporting">· {item.detail}</Text></Text>
      <CopyValue value={item.id} label={`${item.name} ID`} isSecondary />
    </VStack>)}
  </VStack>;
}

/** Guidance next to the manifest editor: the CLI equivalent, the rules review checks, and IDs to use. */
export function ManifestHelp({ kind, mode }: { kind: ManifestKind; mode: ManifestMode }) {
  const references = mode !== "update" && kind !== "CloudAccount";
  return <VStack gap={5} data-manifest-help="">
    <VStack gap={2} as="section" aria-labelledby="manifest-cli">
      <Heading level={3} accessibilityLevel={4} id="manifest-cli">Same as the CLI</Heading>
      <CopyValue value={command(kind, mode)} label="CLI command" type="code" />
      <Link href={`https://github.com/yindia/cluster-vending/blob/main/examples/${examples[kind]}`} target="_blank">Annotated example: examples/{examples[kind]}</Link>
    </VStack>
    <VStack gap={2} as="section" aria-labelledby="manifest-rules">
      <Heading level={3} accessibilityLevel={4} id="manifest-rules">Review checks</Heading>
      <List density="compact" edge="flush" hasDividers>
        {rules(kind, mode).map((rule) => <ListItem key={rule.label} label={rule.label} description={rule.description} />)}
      </List>
    </VStack>
    {references && <References kind={kind} />}
  </VStack>;
}
