"use client";

import { useCallback, useId, useState, useSyncExternalStore } from "react";

import type { JsonObject } from "@bufbuild/protobuf";
import { CloudResourceConfiguration } from "./cloud-resource-configuration";
import { FormEntryRows, keyIssues, type FormEntry } from "./form-entry-rows";
import { CloudResourceFields } from "./cloud-resource-fields";
import { ValuesEditor } from "./values-editor";
import { parseComponentValues } from "@/lib/workflows/components";
import { additionalParameters, previewDefaults, previewYaml, validatePreviewParameters } from "@/lib/providers/aws-preview-parameters";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { RadioList, RadioListItem } from "@astryxdesign/core/RadioList";
import { Grid } from "@astryxdesign/core/Grid";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { awsPreviewServices, isPreviewService, readPreviews, type AwsResourcePreview, type BucketControls, type PreviewService } from "@/lib/providers/aws-resource-previews";
import { ConsoleTable } from "./console-table";
import { WizardDialog } from "./wizard-dialog";

type Scope = { clusterId: string } & (
  | { ownership: "platform"; subtenantId?: never }
  | { ownership: "subtenant"; subtenantId: string }
);
const previewEvent = "cv:managed-resource-previews";
const empty = () => "[]";
function subscribe(update: () => void) {
  window.addEventListener(previewEvent, update);
  return () => window.removeEventListener(previewEvent, update);
}

function usePreviews({ clusterId, ownership, subtenantId }: Scope) {
  const { identity, tenantId } = usePlatform();
  const key = JSON.stringify([ownership === "platform" ? "cv.platform-resource-previews.v1" : "cv.resource-previews.v1", identity?.anonymous, identity?.issuer, identity?.subject, identity?.username, tenantId, clusterId, subtenantId]);
  const snapshot = useCallback(() => {
    try { return sessionStorage.getItem(key) ?? "[]"; } catch { return ""; }
  }, [key]);
  const serialized = useSyncExternalStore(subscribe, snapshot, empty);
  let rows: AwsResourcePreview[] = [];
  let error = "";
  try { rows = readPreviews(serialized); } catch { error = "Local previews are unavailable. Check browser tab storage and refresh."; }

  function append(preview: AwsResourcePreview) {
    const latest = readPreviews(snapshot());
    if (latest.some(row => row.name === preview.name && row.service === preview.service)) {
      throw new Error("A preview for this service already uses that name for this owner.");
    }
    try { sessionStorage.setItem(key, JSON.stringify([...latest, preview])); }
    catch { throw new Error("The preview could not be saved in this browser tab."); }
    window.dispatchEvent(new Event(previewEvent));
  }
  return { rows, error, append };
}

export function ManagedResourcePreviews(props: Scope & { owner: string; region: string; provider?: string; deleting: boolean }) {
  const { owner, region, provider, deleting, ownership } = props;
  const ownerLabel = ownership === "platform" ? "Platform" : "Sub Tenant";
  const ownerDescription = ownerLabel + " · " + owner;
  const { tenantId } = usePlatform();
  const [inspected, setInspected] = useState<AwsResourcePreview>();
  const { rows, error, append } = usePreviews(props);
  const [creating, setCreating] = useState(false);
  const [notice, setNotice] = useState("");
  const allowCreate = provider === "aws" && !deleting && !error;
  const heading = ownership === "platform" ? "Platform-owned cloud resources" : "Sub Tenant cloud resources";
  return <VStack as="section" aria-label={heading} gap={4}>
    <HStack hAlign="between" vAlign="center" gap={3} wrap="wrap">
      <Heading level={2}>{heading}</Heading>
      {allowCreate && <Button label="Create cloud resource" variant="primary" onClick={() => setCreating(true)} />}
    </HStack>
    <Text color="secondary">{ownership === "platform" ? "Owned by the platform for this cluster." : `Owned by the Sub Tenant ${owner}.`} Previews stay in this browser tab and do not provision AWS resources.</Text>
    {error && <Banner status="warning" title="Storage unavailable" description={error} />}
    {notice && <Text role="status">{notice}</Text>}
    {provider !== "aws" && <Text>Previews are available for AWS clusters.</Text>}
    {deleting && <Text>This owner is being deleted.</Text>}
    {!error && <ConsoleTable aria-label="Managed resource previews" data={rows.map(row => ({ ...row, id: row.service + "/" + row.name }))} idKey="id" emptyState="No managed resource previews yet." hasFullDetails columns={[
      { key: "name", header: "Resource", resizable: false, renderCell: row => <VStack gap={1}><Text weight="medium" maxLines={1} hasTruncateTooltip>{row.name}</Text><Text type="supporting">{awsPreviewServices[row.service].label}</Text></VStack> },
      { key: "settings", header: "Configuration", resizable: false, renderCell: row => row.parameters ? <Button label="View YAML" variant="ghost" size="sm" onClick={() => setInspected(row)} /> : <Text>{row.settings?.join(" / ")}</Text> },
    ]} />}
    {inspected && <WizardDialog isOpen title={inspected.name} steps={["Crossplane preview"]} step={0} onOpenChange={open => { if (!open) setInspected(undefined); }}><ValuesEditor label="Saved Crossplane preview" description={ownerDescription + " · Local preview"} readOnly value={previewYaml(inspected, tenantId, props.clusterId, props.subtenantId)} /></WizardDialog>}
    {allowCreate && creating && <PreviewWizard scope={props} owner={ownerDescription} region={region} existing={rows} onClose={() => setCreating(false)} onSave={preview => {
      append(preview);
      setCreating(false);
      setNotice(`Saved ${preview.name} for ${ownerDescription} in this tab.`);
    }} />}
  </VStack>;
}

function PreviewWizard({ scope, owner, region, existing, onClose, onSave }: { scope: Scope; owner: string; region: string; existing: AwsResourcePreview[]; onClose: () => void; onSave: (preview: AwsResourcePreview) => void }) {
  const formId = useId();
  const { tenantId } = usePlatform();
  const [step, setStep] = useState(0);
  const [service, setService] = useState<PreviewService>("s3");
  const [name, setName] = useState("");
  const [selectedRegion, setSelectedRegion] = useState(region);
  const [drafts, setDrafts] = useState<Partial<Record<PreviewService, JsonObject>>>({});
  const [tags, setTags] = useState<FormEntry[]>([]);
  const [versioning, setVersioning] = useState(true);
  const [bucket, setBucket] = useState<BucketControls>({ encryption: "AES256", kmsKeyId: "", blockPublicAccess: true });
  const [error, setError] = useState("");
  const [overrides, setOverrides] = useState<Partial<Record<PreviewService, string>>>({});
  const [reviewed, setReviewed] = useState<AwsResourcePreview>();
  const definition = awsPreviewServices[service];
  const fields = drafts[service] ?? previewDefaults(service);
  function changeField(key: string, value: string | string[] | number | boolean | null) {
    setDrafts(previous => {
      const next = { ...(previous[service] ?? previewDefaults(service)) };
      if (value === "" || value === null) delete next[key];
      else next[key] = value;
      return { ...previous, [service]: next };
    });
    setError("");
  }

  function submit() {
    try {
      if (step === 2 && reviewed) { onSave(reviewed); return; }
      const resourceName = name.trim();
      if (step === 0) {
        if (!resourceName || resourceName.length > 253 || resourceName.split(".").some(part => !/^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(part))) {
          throw new Error("Use a Kubernetes resource name with lowercase letters, numbers, hyphens or dots.");
        }
        if (!selectedRegion.trim()) throw new Error("Enter the AWS region.");
        if (existing.some(row => row.name === resourceName && row.service === service)) throw new Error("A preview for this service already uses that name.");
      } else {
        const issues = keyIssues(tags);
        if (issues.size) throw new Error([...issues.values()][0]);
        const yaml = overrides[service]?.trim();
        const parameters: JsonObject = {
          ...fields, region: selectedRegion.trim(),
          ...(service === "elasticache" ? { description: resourceName } : {}),
          ...(tags.length ? { tags: Object.fromEntries(tags.map(row => [row.key, row.value])) } : {}),
          ...(yaml ? additionalParameters(parseComponentValues(yaml) ?? {}) : {}),
        };
        validatePreviewParameters(service, parameters);
        if (service === "s3" && parameters.objectLockEnabled && !versioning) throw new Error("Enable object versioning to use Object Lock.");
        if (service === "elasticache" && parameters.automaticFailoverEnabled && parameters.numCacheClusters === 1) throw new Error("Automatic failover requires at least two cache nodes.");
        setReviewed({ name: resourceName, service, parameters, ...(service === "s3" ? { versioning, bucketControls: bucket } : {}) });
      }
      setError("");
      setStep(step + 1);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "The preview could not be saved.");
    }
  }
  return <WizardDialog isOpen title="Create cloud resource" description={owner} steps={["Resource", definition.label + " settings", "Review preview"]} step={step} onOpenChange={open => { if (!open) onClose(); }}
    footer={<HStack gap={2} wrap="wrap">
      <Button label="Cancel" onClick={onClose} />
      {step > 0 && <Button label="Back" onClick={() => { setStep(step - 1); setError(""); }} />}
      <Button type="submit" form={formId} variant="primary" label={["Configure resource", "Review preview", "Create preview"][step]} />
    </HStack>}>
    <form id={formId} onSubmit={event => { event.preventDefault(); submit(); }}>
      <VStack gap={5}>
        {error && <Banner role="alert" status="error" title="Check the preview" description={error} />}
        {step === 0 && <>
          <RadioList label="AWS service" htmlName="service" value={service} onChange={value => { if (isPreviewService(value)) { setService(value); setError(""); } }}>
            {Object.entries(awsPreviewServices).map(([value, def]) => <RadioListItem key={value} value={value} label={def.label} description={def.description} />)}
          </RadioList>
          <Grid columns={{ minWidth: 210, max: 2 }} gap={4}>
            <TextInput label="Resource name" htmlName="resourceName" isRequired value={name} onChange={value => { setName(value); setError(""); }} autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} autoComplete="off" />
            <TextInput label="AWS region" htmlName="region" isRequired value={selectedRegion} onChange={value => { setSelectedRegion(value); setError(""); }} autoCapitalize="none" hasSpellCheck={false} />
          </Grid>
        </>}
        {step === 1 && <>
          <CloudResourceConfiguration service={service} fields={fields} change={changeField} versioning={versioning} setVersioning={setVersioning} bucket={bucket} setBucket={setBucket} />
          <FormEntryRows label="Tags" rowLabel="tag" rows={tags} onChange={rows => { setTags(rows); setError(""); }} disabled={false} issues={error ? keyIssues(tags) : undefined} />
          <CloudResourceFields service={service} value={overrides[service] ?? ""} onChange={value => { setOverrides(previous => ({ ...previous, [service]: value })); setError(""); }} />
        </>}
        {step === 2 && reviewed && <>
          <MetadataList>
            <MetadataListItem label="Owner">{owner}</MetadataListItem>
            <MetadataListItem label="Service">{definition.label}</MetadataListItem>
            <MetadataListItem label="Resource name">{reviewed.name}</MetadataListItem>
            <MetadataListItem label="Region">{String(reviewed.parameters?.region)}</MetadataListItem>
          </MetadataList>
          <ValuesEditor label="Crossplane preview" readOnly value={previewYaml(reviewed, tenantId, scope.clusterId, scope.subtenantId)} description="The account ProviderConfig is a placeholder in this local preview." />
          <Text color="secondary">This preview is saved in this browser tab.</Text>
        </>}
      </VStack>
    </form>
  </WizardDialog>;
}
