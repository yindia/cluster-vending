"use client";

import { useState } from "react";
import type { DescEnum } from "@bufbuild/protobuf";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Heading } from "@astryxdesign/core/Heading";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Selector } from "@astryxdesign/core/Selector";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { AwsAmiTypeSchema, AwsAuthenticationModeSchema, AwsClusterLogTypeSchema, AwsSupportTypeSchema } from "@/gen/cloud/v1/aws_pb";
import { Weekday, type Cluster } from "@/gen/cloud/v1/cluster_pb";
import { TaintEffectSchema } from "@/gen/cloud/v1/node_pool_pb";
import type { ClusterForm } from "@/lib/workflows/clusters";
import { ValuesEditor } from "./values-editor";

const title = (value: string) => value.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase());
const enumOptions = (schema: DescEnum, numeric = true) => schema.values.map((value) => ({ value: numeric ? value.number ? String(value.number) : "" : value.name, label: value.number ? title(value.localName) : "Server default" }));
const weekdays = [Weekday.MONDAY, Weekday.TUESDAY, Weekday.WEDNESDAY, Weekday.THURSDAY, Weekday.FRIDAY, Weekday.SATURDAY, Weekday.SUNDAY];
const enumLabel = (schema: DescEnum, value: string) => { const entry = schema.values.find((entry) => entry.name === value || String(entry.number) === value); return !value || !entry?.number ? "Server default" : title(entry.localName); };
const tokens = (value: string) => value.split(/[\s,]+/).filter(Boolean);
type RowField = "labels" | "nodeLabels" | "nodeTaints";
type Entry = { id: string; key: string; value: string; effect: string };
function taintEffectName(value: string | number | undefined): string {
  const effect = value ?? "TAINT_EFFECT_NO_SCHEDULE";
  return TaintEffectSchema.values.find((entry) => entry.name === effect || String(entry.number) === String(effect))?.name ?? String(effect);
}
function entries(value: string, taints = false): Entry[] {
  try {
    const parsed = JSON.parse(value);
    const rows: { key: string; value: string; effect?: string | number }[] = taints ? parsed : Object.entries(parsed).map(([key, value]) => ({ key, value: String(value) }));
    return rows.map((row) => ({ id: crypto.randomUUID(), key: row.key, value: row.value ?? "", effect: taintEffectName(row.effect) }));
  } catch { return []; }
}
/** UI row identities survive key edits, wizard steps and creation mode changes. */
export function useClusterFieldRows(form: ClusterForm) {
  const [rows, setRows] = useState<Record<RowField, Entry[]>>(() => ({ labels: entries(form.labels), nodeLabels: entries(form.nodeLabels), nodeTaints: entries(form.nodeTaints, true) }));
  function validate(step: number) {
    for (const field of step === 0 ? ["labels"] as const : step === 1 ? ["nodeLabels", "nodeTaints"] as const : []) {
      const values = rows[field];
      if (values.some((row) => !row.key.trim())) throw new Error(`Enter a key for every ${field === "labels" ? "cloud tag" : field === "nodeLabels" ? "node label" : "node taint"}, or remove its empty row.`);
      const keys = values.map((row) => field === "nodeTaints" ? `${row.key}\u0000${row.effect}` : row.key);
      if (new Set(keys).size !== keys.length) throw new Error(field === "nodeTaints" ? "Node taints must use unique key and effect pairs." : "Tag and label keys must be unique.");
    }
  }
  return { rows, setRows, validate };
}
type RowState = ReturnType<typeof useClusterFieldRows>;
type Change = <K extends keyof ClusterForm>(field: K, value: ClusterForm[K]) => void;

function KeyValueRows({ field, label, rowLabel, state, onChange, disabled, taints = false }: { field: RowField; label: string; rowLabel: string; state: RowState; onChange: Change; disabled: boolean; taints?: boolean }) {
  const rows = state.rows[field];
  function update(next: Entry[]) {
    state.setRows((previous) => ({ ...previous, [field]: next }));
    onChange(field, JSON.stringify(taints ? next.map(({ key, value, effect }) => ({ key, value, effect })) : Object.fromEntries(next.map(({ key, value }) => [key, value]))));
  }
  function change(id: string, patch: Partial<Entry>) { update(rows.map((row) => row.id === id ? { ...row, ...patch } : row)); }
  return <VStack gap={3} role="group" aria-label={label}>
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%"><Heading level={4}>{label}</Heading><Button label={`Add ${rowLabel}`} type="button" onClick={() => update([...rows, { id: crypto.randomUUID(), key: "", value: "", effect: "TAINT_EFFECT_NO_SCHEDULE" }])} isDisabled={disabled} /></HStack>
    {!rows.length && <Text type="supporting">No {label.toLowerCase()} configured.</Text>}
    {rows.map((row, index) => <VStack key={row.id} gap={2}>
      <FormLayout direction="horizontal">
        <TextInput label={`${rowLabel[0].toUpperCase() + rowLabel.slice(1)} ${index + 1} key`} htmlName={`${field}-${row.id}-key`} value={row.key} onChange={(value) => change(row.id, { key: value })} isDisabled={disabled} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
        <TextInput label={`${rowLabel[0].toUpperCase() + rowLabel.slice(1)} ${index + 1} value`} htmlName={`${field}-${row.id}-value`} value={row.value} onChange={(value) => change(row.id, { value })} isDisabled={disabled} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />
        {taints && <Selector label={`Node taint ${index + 1} effect`} value={row.effect} onChange={(effect) => change(row.id, { effect })} options={enumOptions(TaintEffectSchema, false).filter((option) => option.value !== "TAINT_EFFECT_UNSPECIFIED")} isDisabled={disabled} />}
      </FormLayout>
      <HStack hAlign="end"><Button label={`Remove ${rowLabel} ${index + 1}`} type="button" variant="ghost" onClick={() => update(rows.filter((entry) => entry.id !== row.id))} isDisabled={disabled} /></HStack>
    </VStack>)}
  </VStack>;
}

export function ClusterExtraFields({ value, onChange, disabled, creating, step, rows, baseline }: { value: ClusterForm; onChange: Change; disabled: boolean; creating: boolean; step: number; rows: RowState; baseline?: Cluster }) {
  const encryption = baseline?.spec?.controlPlane?.secretsEncryption;
  const encryptionLocked = !!encryption?.enabled || !!encryption?.kmsKeyArn;
  const text = (field: keyof ClusterForm, label: string, description?: string, immutable = false) => <TextInput label={label} htmlName={field} value={String(value[field])} onChange={(next) => { if (!immutable || creating) onChange(field, next); }} description={immutable && !creating ? "Set at creation; cannot be changed here." : description} isReadOnly={immutable && !creating} isDisabled={disabled} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />;
  const choice = (field: keyof ClusterForm, label: string, options: { label: string; value: string }[], locked = false) => <Selector label={label} value={String(value[field])} onChange={(next) => { if (!locked) onChange(field, next); }} options={options} isDisabled={disabled || locked} />;
  const booleanOptions = [{ value: "", label: "Server default" }, { value: "true", label: "Enabled" }, { value: "false", label: "Disabled" }];
  const yaml = (field: "oidcYaml" | "accessEntriesYaml" | "launchTemplateYaml" | "addonsYaml", label: string, description: string) => <ValuesEditor label={label} value={value[field]} onChange={(next) => onChange(field, next)} disabled={disabled} description={description} language="yaml" />;
  return <VStack gap={4}>
    {step === 0 && <>
      <Collapsible trigger="Cloud tags" defaultIsOpen={false}><VStack gap={3} paddingBlock={3}><Text type="supporting">Cloud tags apply to the cluster and its managed resources.</Text><KeyValueRows field="labels" label="Cloud tags" rowLabel="cloud tag" state={rows} onChange={onChange} disabled={disabled} /></VStack></Collapsible>
      <Collapsible trigger="Maintenance window" defaultIsOpen={false}><VStack gap={4} paddingBlock={3}>
        <Text type="supporting">Weekly upgrade window in UTC. Leave start and duration empty for no restriction. No selected weekdays means every day.</Text>
        <HStack gap={3} wrap="wrap" role="group" aria-label="Maintenance weekdays">{weekdays.map((day) => <CheckboxInput key={day} label={title(Weekday[day])} value={tokens(value.maintenanceDays).includes(String(day))} isDisabled={disabled} onChange={(checked) => onChange("maintenanceDays", (checked ? [...tokens(value.maintenanceDays), String(day)] : tokens(value.maintenanceDays).filter((entry) => entry !== String(day))).join(","))} />)}</HStack>
        <FormLayout direction="horizontal">{text("maintenanceStart", "Maintenance start (UTC)", "HH:MM, for example 02:00.")}{text("maintenanceDuration", "Maintenance duration", "1–24 hours, for example 4h or 90m.")}</FormLayout>
        <HStack hAlign="end"><Button label="Clear maintenance window" type="button" variant="ghost" isDisabled={disabled} onClick={() => { onChange("maintenanceDays", ""); onChange("maintenanceStart", ""); onChange("maintenanceDuration", ""); }} /></HStack>
      </VStack></Collapsible>
    </>}
    {step === 1 && <>
      {text("desiredSize", "Initial desired nodes", "Optional. Used only at creation; subsequent capacity is managed by the autoscaler.", true)}
      <Collapsible trigger="Node labels and taints" defaultIsOpen={false}><VStack gap={5} paddingBlock={3}>
        <KeyValueRows field="nodeLabels" label="Node labels" rowLabel="node label" state={rows} onChange={onChange} disabled={disabled} />
        <KeyValueRows field="nodeTaints" label="Node taints" rowLabel="node taint" state={rows} onChange={onChange} disabled={disabled} taints />
      </VStack></Collapsible>
      <Collapsible trigger="AWS node image and launch template" defaultIsOpen={false}><VStack gap={4} paddingBlock={3}>
        {choice("awsAmiType", "AWS AMI type", enumOptions(AwsAmiTypeSchema))}
        {yaml("launchTemplateYaml", "AWS launch template (YAML)", "An existing template (id or name plus version) or rendered template settings. Leave empty to remove the template. Some changes replace the node group; inspect the server plan.")}
      </VStack></Collapsible>
    </>}
    {step === 2 && <>
      <Collapsible trigger="Cluster network settings" defaultIsOpen={false}><VStack gap={4} paddingBlock={3}>{text("zoneIds", "Availability zone IDs", "Optional comma-separated zone IDs, for example use1-az1,use1-az2. Set only at creation.", true)}{text("serviceCidr", "Kubernetes service CIDR", "Optional IPv4 service range, /12 to /24 for AWS. Set only at creation.", true)}{text("awsAdditionalSecurityGroups", "Additional cluster security groups", "Comma-separated security group IDs, at most four.", true)}</VStack></Collapsible>
      <Collapsible trigger="Secrets encryption" defaultIsOpen={false}><VStack gap={4} paddingBlock={3}>
        {choice("encryptionEnabled", "Encrypt Kubernetes Secrets", booleanOptions, encryptionLocked)}
        <TextInput label="Encryption KMS key ARN" htmlName="encryptionKeyArn" value={value.encryptionKeyArn} onChange={(next) => { if (!encryptionLocked) onChange("encryptionKeyArn", next); }} isReadOnly={encryptionLocked} isDisabled={disabled} autoComplete="off" description={encryptionLocked ? "Encryption is already enabled. Its key cannot be changed or encryption disabled." : "Optional existing key ARN. An ARN enables encryption; leave empty to let the platform create a key when encryption is enabled."} />
      </VStack></Collapsible>
      <Collapsible trigger="OIDC and access entries" defaultIsOpen={false}><VStack gap={4} paddingBlock={3}>
        {choice("awsAuthenticationMode", "AWS authentication mode", enumOptions(AwsAuthenticationModeSchema), !creating)}
        {!creating && <Text type="supporting">Authentication mode is set at creation and cannot be updated by this API.</Text>}
        {yaml("oidcYaml", "OIDC provider (YAML)", "An OIDC configuration object. Leave empty for no OIDC provider. Settings are validated before review.")}
        {yaml("accessEntriesYaml", "Access entries (YAML)", "An array of access entry objects. Use [] to remove entries. Review removals before applying.")}
      </VStack></Collapsible>
      <Collapsible trigger="AWS support and control plane logging" defaultIsOpen={false}><VStack gap={4} paddingBlock={3}>
        {choice("awsSupportType", "AWS support policy", enumOptions(AwsSupportTypeSchema))}
        {choice("awsLogGroupEnabled", "Managed control plane log group", booleanOptions)}
        {text("awsLogRetentionDays", "Control plane log retention (days)", "Optional supported CloudWatch retention period.")}
        <VStack gap={3} role="group" aria-label="Control plane log types"><Heading level={4}>Control plane log types</Heading>{AwsClusterLogTypeSchema.values.filter((entry) => entry.number).map((entry) => <CheckboxInput key={entry.name} label={title(entry.localName)} value={tokens(value.awsLogTypes).includes(String(entry.number))} onChange={(checked) => onChange("awsLogTypes", (checked ? [...tokens(value.awsLogTypes), String(entry.number)] : tokens(value.awsLogTypes).filter((item) => item !== String(entry.number))).join(","))} isDisabled={disabled} />)}</VStack>
      </VStack></Collapsible>
      <Collapsible trigger="AWS add-ons" defaultIsOpen={false}><VStack paddingBlock={3}>{yaml("addonsYaml", "AWS add-ons (YAML)", "An array of add-on objects. Use [] for the default add-ons. Removing optional entries can delete those managed add-ons. Versions are resolved by the server; review the plan before applying.")}</VStack></Collapsible>
    </>}
  </VStack>;
}

export function ClusterExtraReview({ form, creating = false, touched }: { form: ClusterForm; creating?: boolean; touched?: ReadonlySet<string> }) {
  const shown = (fields: string[]) => creating || fields.some((field) => touched?.has(field));
  if (!creating && !["labels", "nodeLabels", "nodeTaints", "maintenanceDays", "maintenanceStart", "maintenanceDuration", "encryptionEnabled", "encryptionKeyArn", "awsAmiType", "awsAdditionalSecurityGroups", "awsAuthenticationMode", "awsSupportType", "awsLogGroupEnabled", "awsLogRetentionDays", "awsLogTypes", "oidcYaml", "accessEntriesYaml", "launchTemplateYaml", "addonsYaml"].some((field) => touched?.has(field))) return null;
  const map = (text: string) => { try { return Object.entries(JSON.parse(text) as Record<string, string>); } catch { return []; } };
  const taints = (() => { try { return JSON.parse(form.nodeTaints) as { key: string; value: string; effect: string | number }[]; } catch { return []; } })();
  const list = (value: string) => value || "None";
  const reviewYaml = (field: "oidcYaml" | "accessEntriesYaml" | "launchTemplateYaml" | "addonsYaml", label: string) => shown([field]) && <Collapsible trigger={`Reviewed ${label}`} defaultIsOpen={false}><ValuesEditor label={`Reviewed ${label}`} value={form[field]} readOnly language="yaml" /></Collapsible>;
  return <VStack gap={4}>
    <MetadataList title={<Heading level={4}>Additional configuration</Heading>}>
      {shown(["labels"]) && <MetadataListItem label="Cloud tags">{map(form.labels).length} · {map(form.labels).map(([key, value]) => `${key}=${value}`).join(", ") || "None"}</MetadataListItem>}
      {shown(["maintenanceDays", "maintenanceStart", "maintenanceDuration"]) && <MetadataListItem label="Maintenance window">{form.maintenanceStart ? `${tokens(form.maintenanceDays).map((day) => title(Weekday[Number(day)])).join(", ") || "Every day"} · ${form.maintenanceStart} UTC · ${form.maintenanceDuration}` : "No restriction"}</MetadataListItem>}
      {shown(["nodeLabels"]) && <MetadataListItem label="Node labels">{map(form.nodeLabels).length} · {map(form.nodeLabels).map(([key, value]) => `${key}=${value}`).join(", ") || "None"}</MetadataListItem>}
      {shown(["nodeTaints"]) && <MetadataListItem label="Node taints">{taints.length} · {taints.map((taint) => `${taint.key}=${taint.value}:${taintEffectName(taint.effect).replace("TAINT_EFFECT_", "")}`).join(", ") || "None"}</MetadataListItem>}
      {creating && <MetadataListItem label="Initial desired nodes">{form.desiredSize || "Server default"}</MetadataListItem>}
      {creating && <MetadataListItem label="Cluster network">Zones: {list(form.zoneIds)} · Service CIDR: {form.serviceCidr || "Server default"}</MetadataListItem>}
      {shown(["encryptionEnabled", "encryptionKeyArn"]) && <MetadataListItem label="Secrets encryption">{form.encryptionKeyArn || (form.encryptionEnabled === "true" ? "Enabled · platform key" : form.encryptionEnabled === "false" ? "Disabled" : "Server default")}</MetadataListItem>}
      {shown(["awsAmiType", "awsAdditionalSecurityGroups"]) && <MetadataListItem label="AWS settings">AMI: {enumLabel(AwsAmiTypeSchema, form.awsAmiType)} · Security groups: {list(form.awsAdditionalSecurityGroups)}</MetadataListItem>}
      {shown(["awsAuthenticationMode", "awsSupportType"]) && <MetadataListItem label="AWS policies">Authentication: {enumLabel(AwsAuthenticationModeSchema, form.awsAuthenticationMode)} · Support: {enumLabel(AwsSupportTypeSchema, form.awsSupportType)}</MetadataListItem>}
      {shown(["awsLogGroupEnabled", "awsLogRetentionDays", "awsLogTypes"]) && <MetadataListItem label="Control plane logging">Log group: {form.awsLogGroupEnabled === "true" ? "Enabled" : form.awsLogGroupEnabled === "false" ? "Disabled" : "Server default"} · Retention: {form.awsLogRetentionDays || "Server default"} days · Types: {tokens(form.awsLogTypes).map((value) => enumLabel(AwsClusterLogTypeSchema, value)).join(", ") || "None"}</MetadataListItem>}
    </MetadataList>
    {reviewYaml("oidcYaml", "OIDC provider")}{reviewYaml("accessEntriesYaml", "access entries")}{reviewYaml("launchTemplateYaml", "AWS launch template")}{reviewYaml("addonsYaml", "AWS add-ons")}
  </VStack>;
}
