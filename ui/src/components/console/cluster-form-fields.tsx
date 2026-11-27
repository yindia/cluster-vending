"use client";

import { useState, type ReactNode } from "react";
import type { DescEnum } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Divider } from "@astryxdesign/core/Divider";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Selector } from "@astryxdesign/core/Selector";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { AwsAuthenticationModeSchema, AwsClusterLogTypeSchema, AwsSupportType, AwsSupportTypeSchema } from "@/gen/cloud/v1/aws_pb";
import { UpgradeMode, Weekday, type Cluster } from "@/gen/cloud/v1/cluster_pb";
import type { ClusterForm } from "@/lib/workflows/clusters";
import { extendedSupportFields, extendedSupportIssue, isExtendedVersion, type KubernetesVersionOption } from "@/lib/workflows/kubernetes-versions";
import { FormEntryRows, keyIssues, type FormEntry } from "./form-entry-rows";
import { TokenListInput } from "./token-list-input";
import { AccessEntriesFields, OidcFields } from "./cluster-access-fields";
import { AddonsFields } from "./cluster-aws-fields";
import { tierLabel } from "./cluster-network-picker";
import { KubernetesVersionField, useKubernetesOffer } from "./cluster-version-field";

export const title = (value: string) => value.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase());
export const enumOptions = (schema: DescEnum, defaultLabel = "Server default") => schema.values.map((value) => ({ value: value.number ? String(value.number) : "", label: value.number ? title(value.localName) : defaultLabel }));
export const enumLabel = (schema: DescEnum, value: string) => { const entry = schema.values.find((item) => item.name === value || String(item.number) === value); return !value || !entry?.number ? "Server default" : title(entry.localName); };
export const tokens = (value: string) => value.split(/[\s,]+/).filter(Boolean);
const weekdays = [Weekday.MONDAY, Weekday.TUESDAY, Weekday.WEDNESDAY, Weekday.THURSDAY, Weekday.FRIDAY, Weekday.SATURDAY, Weekday.SUNDAY];
const booleanOptions = [{ value: "", label: "Server default" }, { value: "true", label: "Enabled" }, { value: "false", label: "Disabled" }];
/** Node groups are not settings of a cluster: the platform creates them from its own configuration. */
export const platformNodeGroupsNote = "Managed by the platform: it creates the system node group and the Karpenter platform pool from its configuration. They show on the cluster's Node groups tab.";
const fixedNote = "Set when the cluster is created; it cannot be changed.";
/** Only the platform's role is admin by default; the console, unlike the CLI, does not add the caller. */
export const adminPrincipalsNote = "IAM role or user ARNs that get cluster-admin. Only the platform's own role is an administrator by default: add your own IAM role or user (the identity of your AWS session) to use kubectl on this cluster. Type a value and press Enter.";
const extendedNote = "This Kubernetes minor is on extended support: the release offers it on no channel, so it runs with Manual upgrades and the Extended support policy.";
const plain = { autoComplete: "off", autoCapitalize: "none", hasAutoCorrect: false, hasSpellCheck: false } as const;

function entries(value: string): FormEntry[] {
  try { return Object.entries(JSON.parse(value) as Record<string, string>).map(([key, item]) => ({ id: crypto.randomUUID(), key, value: String(item), choice: "" })); }
  catch { return []; }
}
/** UI row identities of the cloud tags survive key edits and wizard steps; `reset` reloads them after a YAML edit. */
export function useClusterFieldRows(form: ClusterForm) {
  const [rows, setRows] = useState<FormEntry[]>(() => entries(form.labels));
  const [invalidIds, setInvalidIds] = useState<ReadonlySet<string>>(new Set());
  function validate(step: number) {
    if (step !== 0) return;
    const issues = keyIssues(rows);
    setInvalidIds(new Set(issues.keys()));
    const [first] = issues;
    if (first) throw new Error(`Cloud tag ${rows.findIndex((row) => row.id === first[0]) + 1}: ${first[1]}`);
  }
  function reset(next: ClusterForm) { setRows(entries(next.labels)); setInvalidIds(new Set()); }
  return { rows, setRows, validate, reset, invalidIds };
}
type RowState = ReturnType<typeof useClusterFieldRows>;
type Change = <K extends keyof ClusterForm>(field: K, value: ClusterForm[K]) => void;

function Section({ title: heading, description, children }: { title: string; description?: string; children?: ReactNode }) {
  return <VStack gap={4} as="section" aria-label={heading}>
    <Divider />
    <VStack gap={1}><Heading level={3} accessibilityLevel={4}>{heading}</Heading>{description && <Text type="supporting">{description}</Text>}</VStack>
    {children}
  </VStack>;
}

export interface ClusterFieldsProps {
  value: ClusterForm;
  onChange: Change;
  disabled: boolean;
  creating?: boolean;
  naasLockReason?: string;
  rows: RowState;
  /** Editing: the stored cluster (as requested) and the cluster as it runs. */
  baseline?: Cluster;
  current?: Cluster;
  networkPicker?: ReactNode;
  networkName?: string;
  region: string;
  tier?: number;
  step: number;
}

/** Every ClusterSpec field, grouped by wizard step. Editing shows fields set at creation read-only with the reason. */
export function ClusterFields(props: ClusterFieldsProps) {
  const { step } = props;
  return <VStack gap={5} data-cluster-step={step}>
    {step === 0 && <SettingsStep {...props} />}
    {step === 1 && <AccessStep {...props} />}
    {step === 2 && <OperationsStep {...props} />}
  </VStack>;
}

function useInputs({ value, onChange, disabled, creating }: ClusterFieldsProps) {
  const text = (field: keyof ClusterForm, label: string, description?: string, options: { locked?: string; required?: boolean; placeholder?: string } = {}) => <TextInput
    label={label} htmlName={field} value={String(value[field])} onChange={(next) => { if (!options.locked) onChange(field, next); }}
    description={options.locked ?? description} isRequired={options.required && !options.locked} isReadOnly={!!options.locked} isDisabled={disabled} placeholder={options.placeholder} {...plain} />;
  const choice = (field: keyof ClusterForm, label: string, options: { label: string; value: string; disabled?: boolean; description?: string }[], locked?: string, description?: string) => <Selector label={label} value={String(value[field])}
    onChange={(next) => { if (!locked) onChange(field, next); }} options={options} isDisabled={disabled || !!locked} description={locked ?? description} />;
  /** Comma-separated form lists as token inputs; read-only text when fixed. */
  const list = (field: keyof ClusterForm, label: string, description: string, options: { locked?: string; placeholder?: string; max?: number; error?: string } = {}) => options.locked
    ? text(field, label, description, { locked: options.locked })
    : <TokenListInput label={label} values={tokens(String(value[field]))} onChange={(next) => onChange(field, next.join(", "))} disabled={disabled} description={description} placeholder={options.placeholder} maxEntries={options.max} error={options.error} />;
  const fixed = creating ? undefined : fixedNote;
  return { text, choice, list, fixed };
}

function SettingsStep(props: ClusterFieldsProps) {
  const { value, onChange, disabled, creating, networkPicker, networkName, region, tier, naasLockReason, rows, current } = props;
  const { text, choice } = useInputs(props);
  const automatic = Number(value.upgradeMode) === UpgradeMode.AUTOMATIC;
  const running = current?.status?.currentKubernetesVersion || current?.spec?.controlPlane?.kubernetesVersion;
  const days = tokens(value.maintenanceDays);
  const offer = useKubernetesOffer(value.releaseChannel);
  // An edit without a pin stays on the minor it runs: that minor is the one the server checks.
  const extended = isExtendedVersion(offer.options, value.kubernetesVersion || running || "");
  const extendedIssue = extendedSupportIssue(value, offer.options, running);
  const chooseVersion = (version: string, option: KubernetesVersionOption | undefined) => {
    onChange("kubernetesVersion", version);
    if (option?.extended) { onChange("upgradeMode", extendedSupportFields.upgradeMode); onChange("awsSupportType", extendedSupportFields.awsSupportType); }
  };
  const supportOptions = enumOptions(AwsSupportTypeSchema).map((option) => extended && option.value === String(AwsSupportType.STANDARD) ? { ...option, disabled: true, description: extendedNote } : option);
  return <>
    {creating ? <FormLayout>
      <TextInput label="Cluster name" value={value.name} onChange={(next) => onChange("name", next)} placeholder="production-eks" isRequired isDisabled={disabled} {...plain} description="1–40 lowercase letters, digits and hyphens. Start with a letter and end with a letter or digit." />
      {networkPicker}
    </FormLayout> : <VStack data-placement-inline=""><Text type="supporting">{networkName || value.networkId} · {region} · {tierLabel(tier)}</Text></VStack>}
    {text("displayName", "Display name", "Optional name shown in the dashboard.")}
    <Section title="Release policy" description={automatic ? "Kubernetes upgrades follow the selected release channel." : "You choose when to upgrade Kubernetes."}>
      <FormLayout direction="horizontal" data-workflow-fields="">
        <Selector label="Release channel" value={value.releaseChannel} isDisabled={disabled} onChange={(next) => onChange("releaseChannel", next)} options={[
          { value: "0", label: automatic ? "Stable (default)" : "None (default)" }, { value: "1", label: "Alpha" }, { value: "2", label: "Beta" }, { value: "3", label: "Stable" },
        ]} />
        <Selector label="Upgrades" value={value.upgradeMode} isDisabled={disabled} onChange={(next) => onChange("upgradeMode", next)}
          options={[{ value: "1", label: "Manual (default)" }, { value: "2", label: "Automatic", disabled: extended && !automatic, description: extended ? extendedNote : undefined }]} />
      </FormLayout>
      {!automatic && <KubernetesVersionField value={value.kubernetesVersion} offer={offer} running={running} disabled={disabled} onChange={chooseVersion} />}
      {choice("awsSupportType", "AWS support policy", supportOptions, undefined, "Extended support keeps an older Kubernetes version past standard support, at extra cost.")}
      {extended && !extendedIssue && <Text type="supporting" data-extended-support="">{extendedNote} Automatic upgrades and the Standard support policy are unavailable until the cluster moves to a minor on a release channel.</Text>}
      {extendedIssue && <Banner status="warning" title="Extended-support version" description={extendedIssue} />}
    </Section>
    <Section title="Node groups" description={platformNodeGroupsNote} />
    <Section title="Namespace as a Service">
      <CheckboxInput label="Namespace as a Service (NaaS)" htmlName="naas" value={value.naas} isDisabled={disabled || !!naasLockReason} onChange={(next) => onChange("naas", next)} description={naasLockReason || "Enable support for team namespaces with separate access, quotas, and policies."} />
    </Section>
    <Section title="Cloud tags" description="Tags apply to the cluster and its managed resources.">
      <FormEntryRows label="Cloud tags" rowLabel="cloud tag" rows={rows.rows} disabled={disabled} limit={40} hint="Up to 40 AWS tags; the aws: prefix is reserved."
        issues={new Map([...keyIssues(rows.rows)].filter(([id]) => rows.invalidIds.has(id)))}
        onChange={(next) => { rows.setRows(next); onChange("labels", JSON.stringify(Object.fromEntries(next.map(({ key, value: item }) => [key, item])))); }} />
    </Section>
    <Section title="Maintenance window" description="Weekly window in UTC for upgrades the platform starts. Leave start and duration empty for no restriction; no weekday means every day.">
      <HStack gap={3} wrap="wrap" role="group" aria-label="Maintenance weekdays">{weekdays.map((day) => <CheckboxInput key={day} label={title(Weekday[day])} value={days.includes(String(day))} isDisabled={disabled}
        onChange={(checked) => onChange("maintenanceDays", (checked ? [...days, String(day)] : days.filter((entry) => entry !== String(day))).join(","))} />)}</HStack>
      <FormLayout direction="horizontal">{text("maintenanceStart", "Maintenance start (UTC)", "HH:MM, for example 02:00.")}{text("maintenanceDuration", "Maintenance duration", "1–24 hours, for example 4h or 90m.")}</FormLayout>
      <HStack hAlign="end"><Button label="Clear maintenance window" type="button" variant="ghost" isDisabled={disabled} onClick={() => { onChange("maintenanceDays", ""); onChange("maintenanceStart", ""); onChange("maintenanceDuration", ""); }} /></HStack>
    </Section>
  </>;
}

function AccessStep(props: ClusterFieldsProps) {
  const { value, onChange, disabled, creating, baseline } = props;
  const { text, choice, list, fixed } = useInputs(props);
  const encryption = baseline?.spec?.controlPlane?.secretsEncryption;
  const encryptionLocked = !!encryption?.enabled || !!encryption?.kmsKeyArn;
  return <>
    <Section title="API endpoints" description="Control who can reach the Kubernetes API. At least one endpoint must remain enabled.">
      <FormLayout>
        <CheckboxInput label="Private endpoint" description="Reachable from the cluster network." value={value.privateAccess} isDisabled={disabled} onChange={(next) => onChange("privateAccess", next)} />
        <CheckboxInput label="Public endpoint" description="Allow access from specific IPv4 networks." value={value.publicAccess} isDisabled={disabled} onChange={(next) => onChange("publicAccess", next)} />
        {value.publicAccess && list("publicCIDRs", "Allowed IPv4 CIDRs", "Ranges such as 203.0.113.0/24; 0.0.0.0/0 allows any source. Type a value and press Enter.", { placeholder: "203.0.113.0/24", max: 40 })}
      </FormLayout>
      {!value.publicAccess && <Text type="supporting">The management plane must be able to reach the private endpoint to install charts.</Text>}
    </Section>
    <Section title="Cluster access" description="Who may administer the cluster, and how IAM principals map to Kubernetes.">
      {list("adminPrincipals", "Cluster administrators", adminPrincipalsNote, { placeholder: "arn:aws:iam::111122223333:role/admin", max: 20 })}
      {choice("awsAuthenticationMode", "AWS authentication mode", enumOptions(AwsAuthenticationModeSchema, "API · server default"), fixed, "How IAM principals authenticate: EKS access entries (API), the aws-auth ConfigMap, or both.")}
      <AccessEntriesFields value={value.accessEntriesYaml} onChange={(next) => onChange("accessEntriesYaml", next)} disabled={disabled} />
      {choice("irsa", "AWS service account roles (IRSA)", [{ value: "default", label: "Enabled · server default" }, { value: "true", label: "Enabled" }, { value: "false", label: "Disabled" }], undefined, "Lets pods assume IAM roles through the cluster's OIDC issuer.")}
    </Section>
    <Section title="OIDC sign-in">
      <OidcFields value={value.oidcYaml} onChange={(next) => onChange("oidcYaml", next)} disabled={disabled} />
    </Section>
    <Section title="Cluster network" description={creating ? "Optional. Empty fields take the network's zones and the server default service range." : undefined}>
      {list("zoneIds", "Availability zone IDs", "At least two AWS zone IDs, for example use1-az1. Empty: the network's zones.", { locked: fixed, placeholder: "use1-az1" })}
      {text("serviceCidr", "Kubernetes service CIDR", "Optional IPv4 range, /12 to /24. Empty: the server default.", { locked: fixed, placeholder: "Server default" })}
      {list("awsAdditionalSecurityGroups", "Additional cluster security groups", "Up to four security group IDs added to the control plane.", { locked: fixed, placeholder: "sg-0123456789abcdef0", max: 4 })}
    </Section>
    <Section title="Secrets encryption">
      {choice("encryptionEnabled", "Encrypt Kubernetes Secrets", booleanOptions, encryptionLocked ? "Encryption is already enabled. It cannot be disabled." : undefined)}
      <TextInput label="Encryption KMS key ARN" htmlName="encryptionKeyArn" value={value.encryptionKeyArn} onChange={(next) => { if (!encryptionLocked) onChange("encryptionKeyArn", next); }} isReadOnly={encryptionLocked} isDisabled={disabled} {...plain}
        description={encryptionLocked ? "Encryption is already enabled. Its key cannot be changed or encryption disabled." : "Optional existing key ARN. An ARN enables encryption; leave empty to let the platform create a key when encryption is enabled."} />
    </Section>
  </>;
}

function OperationsStep(props: ClusterFieldsProps) {
  const { value, onChange, disabled, current } = props;
  const { text, choice } = useInputs(props);
  const types = tokens(value.awsLogTypes);
  const running = current?.spec?.provider.case === "aws" ? current.spec.provider.value.addons : undefined;
  return <>
    <Section title="Add-ons">
      <AddonsFields value={value.addonsYaml} onChange={(next) => onChange("addonsYaml", next)} disabled={disabled} running={running} />
    </Section>
    <Section title="Control plane logging" description="Which control plane logs EKS sends to CloudWatch, and the log group the platform manages for them.">
      <VStack gap={3} role="group" aria-label="Control plane log types"><Text type="label">Control plane log types</Text>
        <HStack gap={4} wrap="wrap">{AwsClusterLogTypeSchema.values.filter((entry) => entry.number).map((entry) => <CheckboxInput key={entry.name} label={title(entry.localName)} value={types.includes(String(entry.number))}
          onChange={(checked) => onChange("awsLogTypes", (checked ? [...types, String(entry.number)] : types.filter((item) => item !== String(entry.number))).join(","))} isDisabled={disabled} />)}</HStack>
        {!types.length && <Text type="supporting">No log types: control plane logging is off.</Text>}
      </VStack>
      <FormLayout direction="horizontal" data-workflow-fields="">
        {choice("awsLogGroupEnabled", "Managed control plane log group", booleanOptions)}
        {text("awsLogRetentionDays", "Control plane log retention (days)", "Optional supported CloudWatch retention period.", { placeholder: "Server default" })}
      </FormLayout>
    </Section>
  </>;
}
