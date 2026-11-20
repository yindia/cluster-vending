"use client";

import { useState } from "react";
import type { DescEnum } from "@bufbuild/protobuf";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Heading } from "@astryxdesign/core/Heading";
import { FormEntryRows, keyIssues, type FormEntry } from "./form-entry-rows";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Selector } from "@astryxdesign/core/Selector";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { AwsAmiTypeSchema, AwsAuthenticationModeSchema, AwsClusterLogTypeSchema, AwsSupportTypeSchema } from "@/gen/cloud/v1/aws_pb";
import { Weekday, type Cluster } from "@/gen/cloud/v1/cluster_pb";
import { TaintEffectSchema } from "@/gen/cloud/v1/node_pool_pb";
import { emptyClusterForm, type ClusterForm } from "@/lib/workflows/clusters";
import { ValuesEditor } from "./values-editor";

const title = (value: string) => value.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase());
const enumOptions = (schema: DescEnum, numeric = true) => schema.values.map((value) => ({ value: numeric ? value.number ? String(value.number) : "" : value.name, label: value.number ? title(value.localName) : "Server default" }));
const weekdays = [Weekday.MONDAY, Weekday.TUESDAY, Weekday.WEDNESDAY, Weekday.THURSDAY, Weekday.FRIDAY, Weekday.SATURDAY, Weekday.SUNDAY];
const enumLabel = (schema: DescEnum, value: string) => { const entry = schema.values.find((entry) => entry.name === value || String(entry.number) === value); return !value || !entry?.number ? "Server default" : title(entry.localName); };
const tokens = (value: string) => value.split(/[\s,]+/).filter(Boolean);
export const taintEffectOptions = () => enumOptions(TaintEffectSchema, false).filter(option => option.value !== "TAINT_EFFECT_UNSPECIFIED");
type RowField = "labels" | "nodeLabels" | "nodeTaints";
function taintEffectName(value: string | number | undefined): string {
  const effect = value ?? "TAINT_EFFECT_NO_SCHEDULE";
  return TaintEffectSchema.values.find((entry) => entry.name === effect || String(entry.number) === String(effect))?.name ?? String(effect);
}
function entries(value: string, taints = false): FormEntry[] {
  try {
    const parsed = JSON.parse(value);
    const rows: { key: string; value: string; effect?: string | number }[] = taints ? parsed : Object.entries(parsed).map(([key, value]) => ({ key, value: String(value) }));
    return rows.map((row) => ({ id: crypto.randomUUID(), key: row.key, value: row.value ?? "", choice: taintEffectName(row.effect) }));
  } catch { return []; }
}
/** Read-only view of the platform-managed default node group's labels and taints. */
function lockedEntries(value: string, taints: boolean) {
  const items = entries(value, taints).map(row => taints ? `${row.key}=${row.value}:${row.choice.replace("TAINT_EFFECT_", "")}` : `${row.key}=${row.value}`);
  return items.length ? <VStack gap={1}>{items.map(item => <Text key={item} type="code">{item}</Text>)}</VStack> : "None";
}
/** UI row identities survive key edits, wizard steps and creation mode changes. */
export function useClusterFieldRows(form: ClusterForm) {
  const [rows, setRows] = useState<Record<RowField, FormEntry[]>>(() => ({ labels: entries(form.labels), nodeLabels: entries(form.nodeLabels), nodeTaints: entries(form.nodeTaints, true) }));
  const [invalidIds, setInvalidIds] = useState<ReadonlySet<string>>(new Set());
  function validate(step: number) {
    const invalid = new Set<string>();
    let message = "";
    for (const field of step === 0 ? ["labels"] as const : step === 1 ? ["nodeLabels", "nodeTaints"] as const : []) {
      for (const [id, issue] of keyIssues(rows[field], field === "nodeTaints")) {
        invalid.add(id);
        if (!message) {
          const index = rows[field].findIndex(row => row.id === id);
          const label = field === "labels" ? "Cloud tag" : field === "nodeLabels" ? "Node label" : "Node taint";
          message = `${label} ${index + 1}: ${issue}`;
        }
      }
    }
    setInvalidIds(invalid);
    if (message) throw new Error(message);
  }
  return { rows, setRows, validate, invalidIds };
}
type RowState = ReturnType<typeof useClusterFieldRows>;
type Change = <K extends keyof ClusterForm>(field: K, value: ClusterForm[K]) => void;

function KeyValueRows({ field, label, rowLabel, state, onChange, disabled }: { field: RowField; label: string; rowLabel: string; state: RowState; onChange: Change; disabled: boolean }) {
  const rows = state.rows[field];
  const taints = field === "nodeTaints";
  const issues = state.invalidIds.size ? keyIssues(rows, taints) : new Map<string, string>();
  for (const id of issues.keys()) if (!state.invalidIds.has(id)) issues.delete(id);
  return <FormEntryRows label={label} hint={field === "labels" ? "Tags apply to the cluster and its managed resources." : undefined} rowLabel={rowLabel} rows={rows} disabled={disabled} issues={issues}
    limit={field === "labels" ? 40 : taints ? 50 : 64}
    select={taints ? { label: "Effect", initial: "TAINT_EFFECT_NO_SCHEDULE", options: taintEffectOptions() } : undefined}
    onChange={next => {
      state.setRows(previous => ({ ...previous, [field]: next }));
      onChange(field, JSON.stringify(taints ? next.map(({ key, value, choice }) => ({ key, value, effect: choice })) : Object.fromEntries(next.map(({ key, value }) => [key, value]))));
    }} />;
}

export function ClusterExtraFields({ value, onChange, disabled, creating, step, rows, baseline }: { value: ClusterForm; onChange: Change; disabled: boolean; creating: boolean; step: number; rows: RowState; baseline?: Cluster }) {
  const encryption = baseline?.spec?.controlPlane?.secretsEncryption;
  const encryptionLocked = !!encryption?.enabled || !!encryption?.kmsKeyArn;
  const text = (field: keyof ClusterForm, label: string, description?: string, immutable = false) => <TextInput label={label} htmlName={field} value={String(value[field])} onChange={(next) => { if (!immutable || creating) onChange(field, next); }} description={immutable && !creating ? "Set at creation; cannot be changed here." : description} isReadOnly={immutable && !creating} isDisabled={disabled} autoComplete="off" autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} />;
  const choice = (field: keyof ClusterForm, label: string, options: { label: string; value: string }[], locked = false) => <Selector label={label} value={String(value[field])} onChange={(next) => { if (!locked) onChange(field, next); }} options={options} isDisabled={disabled || locked} />;
  const booleanOptions = [{ value: "", label: "Server default" }, { value: "true", label: "Enabled" }, { value: "false", label: "Disabled" }];
  const yaml = (field: "oidcYaml" | "accessEntriesYaml" | "launchTemplateYaml" | "addonsYaml", label: string, description: string) => <ValuesEditor label={label} value={value[field]} onChange={(next) => onChange(field, next)} disabled={disabled} description={description} language="yaml" />;
  return <VStack gap={4} data-extra-fields="">
    {step === 0 && <>
      <Collapsible trigger="Cloud tags" defaultIsOpen={false}><VStack gap={3} paddingBlock={3}><KeyValueRows field="labels" label="Cloud tags" rowLabel="cloud tag" state={rows} onChange={onChange} disabled={disabled} /></VStack></Collapsible>
      <Collapsible trigger="Maintenance window" defaultIsOpen={false}><VStack gap={4} paddingBlock={3}>
        <Text type="supporting">Weekly upgrade window in UTC. Leave start and duration empty for no restriction. No selected weekdays means every day.</Text>
        <HStack gap={3} wrap="wrap" role="group" aria-label="Maintenance weekdays">{weekdays.map((day) => <CheckboxInput key={day} label={title(Weekday[day])} value={tokens(value.maintenanceDays).includes(String(day))} isDisabled={disabled} onChange={(checked) => onChange("maintenanceDays", (checked ? [...tokens(value.maintenanceDays), String(day)] : tokens(value.maintenanceDays).filter((entry) => entry !== String(day))).join(","))} />)}</HStack>
        <FormLayout direction="horizontal">{text("maintenanceStart", "Maintenance start (UTC)", "HH:MM, for example 02:00.")}{text("maintenanceDuration", "Maintenance duration", "1–24 hours, for example 4h or 90m.")}</FormLayout>
        <HStack hAlign="end"><Button label="Clear maintenance window" type="button" variant="ghost" isDisabled={disabled} onClick={() => { onChange("maintenanceDays", ""); onChange("maintenanceStart", ""); onChange("maintenanceDuration", ""); }} /></HStack>
      </VStack></Collapsible>
    </>}
    {step === 1 && <>
      <Collapsible trigger="Node labels and taints" defaultIsOpen={false}>{creating ? <VStack gap={5} paddingBlock={3}>
        <KeyValueRows field="nodeLabels" label="Node labels" rowLabel="node label" state={rows} onChange={onChange} disabled={disabled} />
        <KeyValueRows field="nodeTaints" label="Node taints" rowLabel="node taint" state={rows} onChange={onChange} disabled={disabled} />
      </VStack> : <VStack gap={3} paddingBlock={3} data-platform-managed="">
        <Text type="supporting">Managed by the platform: the system node group runs Karpenter and the platform&apos;s components. Set labels and taints on a node group instead.</Text>
        <MetadataList>
          <MetadataListItem label="Node labels">{lockedEntries(value.nodeLabels, false)}</MetadataListItem>
          <MetadataListItem label="Node taints">{lockedEntries(value.nodeTaints, true)}</MetadataListItem>
        </MetadataList>
      </VStack>}</Collapsible>
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
  const shown = (fields: (keyof ClusterForm)[]) => fields.some((field) => creating ? form[field] !== emptyClusterForm[field] : touched?.has(field));
  if (!shown(["labels", "nodeLabels", "nodeTaints", "maintenanceDays", "maintenanceStart", "maintenanceDuration", "zoneIds", "serviceCidr", "encryptionEnabled", "encryptionKeyArn", "awsAmiType", "awsAdditionalSecurityGroups", "awsAuthenticationMode", "awsSupportType", "awsLogGroupEnabled", "awsLogRetentionDays", "awsLogTypes", "oidcYaml", "accessEntriesYaml", "launchTemplateYaml", "addonsYaml"])) return null;
  const map = (text: string) => { try { return Object.entries(JSON.parse(text) as Record<string, string>); } catch { return []; } };
  const taints = (() => { try { return JSON.parse(form.nodeTaints) as { key: string; value: string; effect: string | number }[]; } catch { return []; } })();
  const list = (value: string) => value || "None";
  const reviewRows = (items: string[]) => items.length
    ? <VStack gap={1} data-entry-review="">{items.map(item => <Text key={item} type="code">{item}</Text>)}</VStack>
    : creating ? "None" : "Remove all";
  const reviewYaml = (field: "oidcYaml" | "accessEntriesYaml" | "launchTemplateYaml" | "addonsYaml", label: string) => shown([field]) && <Collapsible trigger={`Reviewed ${label}`} defaultIsOpen={false}><ValuesEditor label={`Reviewed ${label}`} value={form[field]} readOnly language="yaml" /></Collapsible>;
  return <VStack gap={4}>
    <MetadataList title={<Heading level={4}>Additional configuration</Heading>}>
      {shown(["labels"]) && <MetadataListItem label="Cloud tags">{reviewRows(map(form.labels).map(([key, value]) => `${key}=${value}`))}</MetadataListItem>}
      {shown(["maintenanceDays", "maintenanceStart", "maintenanceDuration"]) && <MetadataListItem label="Maintenance window">{form.maintenanceStart ? `${tokens(form.maintenanceDays).map((day) => title(Weekday[Number(day)])).join(", ") || "Every day"} · ${form.maintenanceStart} UTC · ${form.maintenanceDuration}` : "No restriction"}</MetadataListItem>}
      {shown(["nodeLabels"]) && <MetadataListItem label="Node labels">{reviewRows(map(form.nodeLabels).map(([key, value]) => `${key}=${value}`))}</MetadataListItem>}
      {shown(["nodeTaints"]) && <MetadataListItem label="Node taints">{reviewRows(taints.map((taint) => `${taint.key}=${taint.value}:${taintEffectName(taint.effect).replace("TAINT_EFFECT_", "")}`))}</MetadataListItem>}
      {shown(["zoneIds", "serviceCidr"]) && <MetadataListItem label="Cluster network">Zones: {list(form.zoneIds)} · Service CIDR: {form.serviceCidr || "Server default"}</MetadataListItem>}
      {shown(["encryptionEnabled", "encryptionKeyArn"]) && <MetadataListItem label="Secrets encryption">{form.encryptionKeyArn || (form.encryptionEnabled === "true" ? "Enabled · platform key" : form.encryptionEnabled === "false" ? "Disabled" : "Server default")}</MetadataListItem>}
      {shown(["awsAmiType", "awsAdditionalSecurityGroups"]) && <MetadataListItem label="AWS settings">AMI: {enumLabel(AwsAmiTypeSchema, form.awsAmiType)} · Security groups: {list(form.awsAdditionalSecurityGroups)}</MetadataListItem>}
      {shown(["awsAuthenticationMode", "awsSupportType"]) && <MetadataListItem label="AWS policies">Authentication: {enumLabel(AwsAuthenticationModeSchema, form.awsAuthenticationMode)} · Support: {enumLabel(AwsSupportTypeSchema, form.awsSupportType)}</MetadataListItem>}
      {shown(["awsLogGroupEnabled", "awsLogRetentionDays", "awsLogTypes"]) && <MetadataListItem label="Control plane logging">Log group: {form.awsLogGroupEnabled === "true" ? "Enabled" : form.awsLogGroupEnabled === "false" ? "Disabled" : "Server default"} · Retention: {form.awsLogRetentionDays || "Server default"} days · Types: {tokens(form.awsLogTypes).map((value) => enumLabel(AwsClusterLogTypeSchema, value)).join(", ") || "None"}</MetadataListItem>}
    </MetadataList>
    {reviewYaml("oidcYaml", "OIDC provider")}{reviewYaml("accessEntriesYaml", "access entries")}{reviewYaml("launchTemplateYaml", "AWS launch template")}{reviewYaml("addonsYaml", "AWS add-ons")}
  </VStack>;
}
