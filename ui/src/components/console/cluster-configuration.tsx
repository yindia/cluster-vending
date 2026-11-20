"use client";

import { toJson, type DescField, type DescMessage, type JsonObject, type JsonValue } from "@bufbuild/protobuf";
import type { ReactNode } from "react";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Divider } from "@astryxdesign/core/Divider";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Heading, Text } from "@astryxdesign/core/Text";
import { ClusterSpecSchema, ReleaseChannel, UpgradeMode, Weekday, type Cluster } from "@/gen/cloud/v1/cluster_pb";
import { getProvider } from "@/lib/providers/registry";
import { CopyValue } from "./copy-value";

type ConfigurationRow = { path: string; label: string; value: string; copy?: boolean };
type ConfigurationGroup = { key: string; title: string; rows: ConfigurationRow[] };
const titles: Record<string, string> = {
  network: "Network", controlPlane: "Control plane", defaultNodePool: "Default node pool",
  access: "Access", maintenanceWindow: "Maintenance window",
  scalingConfig: "Scaling", minSize: "Minimum nodes", maxSize: "Maximum nodes", desiredSize: "Initial desired nodes",
  diskSizeGb: "Disk size (GiB)", naas: "Namespace as a Service", endpointAccess: "API endpoint",
};
const acronyms: Record<string, string> = { aws: "AWS", api: "API", oidc: "OIDC", cidr: "CIDR", cidrs: "CIDRs", id: "ID", ids: "IDs", arn: "ARN", arns: "ARNs", url: "URL", irsa: "IRSA", ami: "AMI", cpu: "CPU", gb: "GB", kms: "KMS", ssh: "SSH" };
function label(name: string): string {
  if (titles[name]) return titles[name];
  return name.replace(/([a-z0-9])([A-Z])/g, "$1 $2").replace(/_/g, " ").split(" ")
    .map((word, index) => acronyms[word.toLowerCase()] ?? (index ? word.toLowerCase() : word[0]?.toUpperCase() + word.slice(1).toLowerCase())).join(" ");
}
function object(value: JsonValue | undefined): value is JsonObject {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
function rows(value: JsonValue | undefined, path: string, name: string, field?: DescField): ConfigurationRow[] {
  if (value === undefined || value === null || value === "") return [];
  // Launch-template user data is configuration, but may contain sensitive input.
  // Convey presence without exposing its payload in a readable metadata row.
  if (field?.name === "user_data" && typeof value === "string" && value) return [{ path, label: name, value: "Configured (user data payload)" }];
  if (Array.isArray(value)) {
    if (!value.length) return [];
    if (value.every(item => !object(item) && !Array.isArray(item)) && !field?.enum && !/(^id$|_ids?$|_arns?$|_url$)/.test(field?.name ?? "")) {
      return [{ path, label: name, value: value.join(", ") }];
    }
    return value.flatMap((item, index) => object(item)
      ? messageRows(item, field?.message, `${path}[${index}]`, `${name} / ${index + 1}`)
      : rows(item, `${path}[${index}]`, `${name} / ${index + 1}`, field));
  }
  if (object(value)) {
    if (field?.fieldKind === "map") {
      const entries = Object.entries(value);
      return entries.flatMap(([key, item]) => rows(item, `${path}.${key}`, `${name} / ${key}`));
    }
    return messageRows(value, field?.message, path, name);
  }
  let display = typeof value === "boolean" ? (value ? "Enabled" : "Disabled") : String(value);
  if (field?.enum && typeof value === "string") {
    if (value.endsWith("_UNSPECIFIED")) return [];
    let prefix = field.enum.values[0]?.name ?? "";
    for (const entry of field.enum.values) {
      while (prefix && !entry.name.startsWith(prefix)) prefix = prefix.slice(0, -1);
    }
    prefix = prefix.slice(0, prefix.lastIndexOf("_") + 1);
    display = label(prefix && value.startsWith(prefix) ? value.slice(prefix.length) : value);
  }
  return [{ path, label: name, value: display, copy: !!value && typeof value === "string" && !!field && /(^id$|_ids?$|_arns?$|_url$)/.test(field.name) }];
}
function messageRows(value: JsonObject, schema: DescMessage | undefined, path: string, prefix = ""): ConfigurationRow[] {
  // Well-known Struct values use their JSON representation, not fields/value
  // implementation details. Unknown provider fields also have a readable fallback.
  const fields = schema && !schema.typeName.startsWith("google.protobuf.") ? schema.fields : undefined;
  if (!fields) {
    const entries = Object.entries(value);
    return entries.flatMap(([key, item]) => rows(item, `${path}.${key}`, prefix ? `${prefix} / ${key}` : key));
  }
  return fields.flatMap((field) => {
    // Add-on inventory belongs to the Components tab; tier is inherited from
    // the network and is not a setting on the cluster.
    if (field.name === "addons" || field.name === "tier") return [];
    if (field.oneof && !Object.hasOwn(value, field.jsonName)) return [];
    const name = prefix ? `${prefix} / ${label(field.jsonName)}` : label(field.jsonName);
    return rows(value[field.jsonName], `${path}.${field.jsonName}`, name, field);
  });
}

/** Walk the generated spec descriptor so advanced/new fields are never whitelisted away. */
export function clusterConfigurationGroups(cluster: Cluster): ConfigurationGroup[] {
  if (!cluster.spec) return [];
  const json = toJson(ClusterSpecSchema, cluster.spec, { alwaysEmitImplicit: true }) as JsonObject;
  const spec = cluster.spec;
  const groups: ConfigurationGroup[] = [{ key: "general", title: "Cluster settings", rows: [
    { path: "name", label: "Name", value: cluster.name, copy: true },
    ...(cluster.displayName && cluster.displayName !== cluster.name ? [{ path: "displayName", label: "Display name", value: cluster.displayName }] : []),
  ] }];
  const automatic = spec.upgradeMode === UpgradeMode.AUTOMATIC;
  groups.push({ key: "upgrades", title: "Upgrades", rows: [
    { path: "upgradeMode", label: "Upgrade mode", value: automatic ? "Automatic" : "Manual" },
    { path: "releaseChannel", label: "Release channel", value: spec.releaseChannel ? label(ReleaseChannel[spec.releaseChannel] ?? String(spec.releaseChannel)) : automatic ? "Stable (default)" : "Not subscribed" },
  ] });
  const window = spec.maintenanceWindow;
  groups.push({ key: "maintenanceWindow", title: "Maintenance window", rows: window ? [
    { path: "maintenanceWindow.days", label: "Days", value: window.days.length ? window.days.map(day => label(Weekday[day] ?? String(day))).join(", ") : "Every day" },
    { path: "maintenanceWindow.start", label: "Start time", value: `${window.start} UTC` },
    { path: "maintenanceWindow.duration", label: "Duration", value: window.duration },
    { path: "maintenanceWindow.applies", label: "Applies to", value: "Automatic upgrades" },
  ] : [{ path: "maintenanceWindow.schedule", label: "Schedule", value: "Any time (no maintenance window configured)" }] });
  for (const field of ClusterSpecSchema.fields) {
    if (["releaseChannel", "upgradeMode", "maintenanceWindow", "tier"].includes(field.jsonName)) continue;
    if (field.oneof && !Object.hasOwn(json, field.jsonName)) continue;
    const nested = field.fieldKind === "message";
    const fieldRows = rows(json[field.jsonName], `spec.${field.jsonName}`, nested ? "" : label(field.jsonName), field);
    if (nested) {
      const provider = field.oneof?.name === "provider";
      if (fieldRows.length) groups.push({ key: field.jsonName, title: provider ? `${getProvider(field.jsonName)?.label ?? label(field.jsonName)} configuration` : label(field.jsonName), rows: fieldRows.map((row) => ({ ...row, label: row.label || label(field.jsonName) })) });
    } else {
      groups[0].rows.push(...fieldRows);
    }
  }
  if (!cluster.spec.provider.case) groups[0].rows.push({ path: "spec.provider", label: "Provider", value: "Not set" });
  else groups[0].rows.push({ path: "spec.provider", label: "Provider", value: getProvider(cluster.spec.provider.case)?.label ?? cluster.spec.provider.case });
  const tags = rows(cluster.labels, "labels", "");
  if (tags.length) groups.push({ key: "labels", title: "Cloud tags", rows: tags.map(row => ({ ...row, label: row.label.replace(/^ \/ /, "") })) });
  const scheduling = new Set(["upgrades", "maintenanceWindow"]);
  return [...groups.filter(group => scheduling.has(group.key)), ...groups.filter(group => !scheduling.has(group.key))];
}

export function ClusterConfiguration({ cluster, actions }: { cluster: Cluster; actions: ReactNode }) {
  const groups = clusterConfigurationGroups(cluster);
  return <VStack gap={6}>
    <HStack hAlign="between" vAlign="center" wrap="wrap" gap={2}><Heading level={2}>Configuration</Heading>{actions}</HStack>
    {!cluster.spec && <Text>No desired configuration was returned.</Text>}
    {groups.map((group) => <VStack key={group.key} gap={4} as="section" aria-label={group.title}>
      <Divider />
      <Heading level={3}>{group.title}</Heading>
      <MetadataList label={{ position: "start", width: "min(40%, 16rem)" }}>{group.rows.map((row) => <MetadataListItem key={row.path} label={row.label}>{row.copy ? <CopyValue value={row.value} label={row.label} /> : <Text wordBreak="break-word">{row.value}</Text>}</MetadataListItem>)}</MetadataList>
    </VStack>)}
  </VStack>;
}
