"use client";

import type { ReactNode } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { Divider } from "@astryxdesign/core/Divider";
import { Heading } from "@astryxdesign/core/Heading";
import { VStack } from "@astryxdesign/core/Layout";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Text } from "@astryxdesign/core/Text";
import { AccessEntryTypeSchema } from "@/gen/cloud/v1/access_pb";
import { AwsAuthenticationModeSchema, AwsClusterLogTypeSchema, AwsSupportTypeSchema } from "@/gen/cloud/v1/aws_pb";
import { UpgradeMode, Weekday } from "@/gen/cloud/v1/cluster_pb";
import { emptyClusterForm, type ClusterForm } from "@/lib/workflows/clusters";
import { enumLabel, title, tokens } from "./cluster-form-fields";
import { tierLabel } from "./cluster-network-picker";
import { enumName, readList, readObject, strings, text } from "./cluster-nested-value";

const code = (items: string[], empty: string): ReactNode => items.length
  ? <VStack gap={1} data-entry-review="">{items.map((item) => <Text key={item} type="code">{item}</Text>)}</VStack>
  : empty;
const pairs = (value: string) => { try { return Object.entries(JSON.parse(value) as Record<string, string>).map(([key, item]) => `${key}=${item}`); } catch { return []; } };

export function ClusterReview({ form, region, networkName, accountName, tier }: { form: ClusterForm; region: string; networkName: string; accountName?: string; tier?: number }) {
  const automatic = Number(form.upgradeMode) === UpgradeMode.AUTOMATIC;
  return <VStack gap={5}>
    <MetadataList columns="multi" title={<Heading level={4}>Placement</Heading>}>
      <MetadataListItem label="Cluster name">{form.name}</MetadataListItem>
      {form.displayName && <MetadataListItem label="Display name">{form.displayName}</MetadataListItem>}
      <MetadataListItem label="Cloud account">{accountName}</MetadataListItem>
      <MetadataListItem label="Network">{networkName}</MetadataListItem>
      <MetadataListItem label="Region / tier">{region} · {tierLabel(tier)}</MetadataListItem>
    </MetadataList>
    <Divider />
    <MetadataList columns="multi" title={<Heading level={4}>Runtime and capacity</Heading>}>
      <MetadataListItem label="Release channel">{[automatic ? "Stable · server default" : "None · server default", "Alpha", "Beta", "Stable"][Number(form.releaseChannel)]}</MetadataListItem>
      <MetadataListItem label="Upgrade policy">{automatic ? "Automatic · no version pin" : `Manual · ${form.kubernetesVersion || "server default version"}`}</MetadataListItem>
      <MetadataListItem label="Node groups">Managed by the platform: the system node group and the Karpenter platform pool</MetadataListItem>
      <MetadataListItem label="Namespace as a Service (NaaS)">{form.naas ? "Enabled" : "Disabled"}</MetadataListItem>
    </MetadataList>
    <Divider />
    <MetadataList columns="multi" title={<Heading level={4}>Access</Heading>}>
      <MetadataListItem label="API endpoints">{[form.privateAccess && "Private", form.publicAccess && "Public"].filter(Boolean).join(" + ")}</MetadataListItem>
      {form.publicAccess && <MetadataListItem label="Allowed sources">{form.publicCIDRs}</MetadataListItem>}
      <MetadataListItem label="Administrators">{form.adminPrincipals || "No additional principals"}</MetadataListItem>
      <MetadataListItem label="Service account roles">{form.irsa === "false" ? "Disabled" : "Enabled"}</MetadataListItem>
    </MetadataList>
    <ClusterExtraReview form={form} creating />
  </VStack>;
}

function accessEntrySummaries(value: string): string[] {
  return (readList(value).value ?? []).map((entry) => {
    const policies = (Array.isArray(entry.policies) ? entry.policies : []) as JsonObject[];
    const type = enumName(AccessEntryTypeSchema, entry.type);
    return `${text(entry.principalArn)}${type ? ` (${title(type.replace("ACCESS_ENTRY_TYPE_", ""))})` : ""}: ${policies.map((policy) => text(policy.policyArn).split("/").at(-1) + (strings(policy.namespaces).length ? ` in ${strings(policy.namespaces).join(", ")}` : "")).join(", ") || "no policies"}`;
  });
}
/**
 * Everything beyond the basics. Creating shows what differs from the defaults; editing (with
 * `changed`) shows only the changed settings, so removals read as "Remove all".
 */
export function ClusterExtraReview({ form, creating = false, changed }: { form: ClusterForm; creating?: boolean; changed?: ReadonlySet<string> }) {
  const shown = (fields: (keyof ClusterForm)[]) => fields.some((field) => creating ? JSON.stringify(form[field]) !== JSON.stringify(emptyClusterForm[field]) : changed?.has(field));
  const empty = creating ? "None" : "Remove all";
  const oidc = readObject(form.oidcYaml).value;
  const addons = readList(form.addonsYaml).value ?? [];
  const rows: [boolean, string, ReactNode][] = [
    [!creating && shown(["displayName"]), "Display name", form.displayName || "None"],
    [!creating && shown(["releaseChannel", "upgradeMode", "kubernetesVersion"]), "Release policy", `${["None", "Alpha", "Beta", "Stable"][Number(form.releaseChannel)]} channel · ${Number(form.upgradeMode) === UpgradeMode.AUTOMATIC ? "automatic" : `manual · ${form.kubernetesVersion || "server default version"}`}`],
    [!creating && shown(["naas"]), "Namespace as a Service (NaaS)", form.naas ? "Enabled" : "Disabled"],
    [shown(["labels"]), "Cloud tags", code(pairs(form.labels), empty)],
    [shown(["maintenanceDays", "maintenanceStart", "maintenanceDuration"]), "Maintenance window", form.maintenanceStart ? `${tokens(form.maintenanceDays).map((day) => title(Weekday[Number(day)] ?? day)).join(", ") || "Every day"} · ${form.maintenanceStart} UTC · ${form.maintenanceDuration}` : "No restriction"],
    [shown(["awsSupportType"]), "AWS support policy", enumLabel(AwsSupportTypeSchema, form.awsSupportType)],
    [!creating && shown(["publicAccess", "privateAccess", "publicCIDRs"]), "API endpoints", `${[form.privateAccess && "Private", form.publicAccess && `Public (${form.publicCIDRs})`].filter(Boolean).join(" + ")}`],
    [!creating && shown(["adminPrincipals"]), "Administrators", code(tokens(form.adminPrincipals), empty)],
    [!creating && shown(["irsa"]), "Service account roles", form.irsa === "false" ? "Disabled" : form.irsa === "true" ? "Enabled" : "Server default"],
    [shown(["awsAuthenticationMode"]), "AWS authentication mode", enumLabel(AwsAuthenticationModeSchema, form.awsAuthenticationMode)],
    [shown(["accessEntriesYaml"]), "Access entries", code(accessEntrySummaries(form.accessEntriesYaml), empty)],
    [shown(["oidcYaml"]), "OIDC provider", oidc && Object.keys(oidc).length ? `${text(oidc.name)} · ${text(oidc.issuerUrl)} · client ${text(oidc.clientId)}` : creating ? "None" : "Remove the provider"],
    [shown(["zoneIds", "serviceCidr", "awsAdditionalSecurityGroups"]), "Cluster network", `Zones: ${form.zoneIds || "the network's"} · Service CIDR: ${form.serviceCidr || "server default"} · Security groups: ${form.awsAdditionalSecurityGroups || "none"}`],
    [shown(["encryptionEnabled", "encryptionKeyArn"]), "Secrets encryption", form.encryptionKeyArn || (form.encryptionEnabled === "true" ? "Enabled · platform key" : form.encryptionEnabled === "false" ? "Disabled" : "Server default")],
    [shown(["addonsYaml"]), "AWS add-ons", code(addons.map((addon) => `${text(addon.name)}${addon.version ? ` ${text(addon.version)}` : " · server-resolved version"}${addon.configurationValues ? " · configured" : ""}`), creating ? "Platform defaults" : "Platform defaults only")],
    [shown(["awsLogGroupEnabled", "awsLogRetentionDays", "awsLogTypes"]), "Control plane logging", `Log group: ${form.awsLogGroupEnabled === "true" ? "Enabled" : form.awsLogGroupEnabled === "false" ? "Disabled" : "Server default"} · Retention: ${form.awsLogRetentionDays ? `${form.awsLogRetentionDays} days` : "server default"} · Types: ${tokens(form.awsLogTypes).map((value) => enumLabel(AwsClusterLogTypeSchema, value)).join(", ") || "None"}`],
  ];
  const visible = rows.filter(([show]) => show);
  if (!visible.length) return null;
  return <MetadataList title={<Heading level={4}>{creating ? "Additional configuration" : "Changed settings"}</Heading>}>
    {visible.map(([, label, value]) => <MetadataListItem key={label} label={label}>{value}</MetadataListItem>)}
  </MetadataList>;
}
