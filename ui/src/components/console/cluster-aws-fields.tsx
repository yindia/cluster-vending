"use client";

import type { JsonObject } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { TextInput } from "@astryxdesign/core/TextInput";
import type { AwsAddon } from "@/gen/cloud/v1/aws_pb";
import { TokenListInput } from "./token-list-input";
import { ValuesEditor } from "./values-editor";
import { readList, strings, text, writeList } from "./cluster-nested-value";

type FieldProps = { value: string; onChange: (value: string) => void; disabled: boolean };
const plain = { autoComplete: "off", autoCapitalize: "none", hasAutoCorrect: false, hasSpellCheck: false } as const;

function Unreadable({ label, error, value, onChange, disabled }: FieldProps & { label: string; error: string }) {
  return <VStack gap={3}>
    <Banner status="warning" title={`${label} could not be read`} description={`${error} Fix the text below, or clear it.`} />
    <ValuesEditor label={`${label} (YAML)`} value={value} onChange={onChange} disabled={disabled} language="yaml" />
    <HStack hAlign="end"><Button type="button" variant="ghost" label={`Clear ${label.toLowerCase()}`} onClick={() => onChange("")} isDisabled={disabled} /></HStack>
  </VStack>;
}

/** spec.aws.addons: EKS add-ons. Empty is the platform's default set with server-resolved versions. */
export function AddonsFields({ value, onChange, disabled, running }: FieldProps & { running?: readonly AwsAddon[] }) {
  const parsed = readList(value);
  if (parsed.error !== undefined) return <Unreadable label="AWS add-ons" error={parsed.error} value={value} onChange={onChange} disabled={disabled} />;
  const addons = parsed.value;
  const write = (next: JsonObject[]) => onChange(writeList(next));
  const patch = (index: number, change: JsonObject) => write(addons.map((addon, position) => position === index ? { ...addon, ...change } : addon));
  return <VStack gap={3} role="group" aria-label="AWS add-ons">
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
      <VStack gap={1}>
        <Heading level={4}>AWS add-ons</Heading>
        <Text type="supporting">The platform installs its default add-ons. List an add-on to pin its version or configure it; leave the version empty for the server-resolved one. Removing an optional add-on deletes it; the plan shows the impact.</Text>
      </VStack>
      <Button type="button" variant="secondary" label="Add AWS add-on" isDisabled={disabled || addons.length >= 12} onClick={() => write([...addons, { name: "" }])} />
    </HStack>
    {!!running?.length && <Text type="supporting" data-running-addons="">Running now: {running.map((addon) => addon.version ? `${addon.name} ${addon.version}` : addon.name).join(", ")}</Text>}
    {!addons.length && <Text type="supporting">No add-ons listed: the platform&apos;s default add-ons at server-resolved versions.</Text>}
    {addons.map((addon, index) => <Card key={index} padding={3}>
      <VStack gap={3} role="group" aria-label={`AWS add-on ${index + 1}`}>
        <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
          <Text type="label">AWS add-on {index + 1}{text(addon.name) ? ` · ${text(addon.name)}` : ""}</Text>
          <Button type="button" variant="ghost" label={`Remove AWS add-on ${index + 1}`} isDisabled={disabled} onClick={() => write(addons.filter((_, position) => position !== index))} />
        </HStack>
        <FormLayout direction="horizontal" data-workflow-fields="">
          <TextInput label={`AWS add-on ${index + 1} name`} value={text(addon.name)} onChange={(next) => patch(index, { name: next })} isRequired isDisabled={disabled} placeholder="metrics-server" {...plain} />
          <TextInput label={`AWS add-on ${index + 1} version`} value={text(addon.version)} onChange={(next) => patch(index, { version: next })} isDisabled={disabled} placeholder="Server-resolved version" {...plain} />
        </FormLayout>
        <TextArea label={`AWS add-on ${index + 1} configuration values`} value={text(addon.configurationValues)} onChange={(next) => patch(index, { configurationValues: next })} isDisabled={disabled} rows={3}
          description="Optional JSON or YAML the add-on accepts, sent as written." hasSpellCheck={false} autoCapitalize="none" />
        <TokenListInput label={`AWS add-on ${index + 1} S3 buckets`} values={strings(addon.s3BucketNames)} onChange={(s3BucketNames) => patch(index, { s3BucketNames })} disabled={disabled} placeholder="bucket-name"
          description="Only for the Mountpoint for Amazon S3 CSI driver: the buckets it may mount." maxEntries={10} />
      </VStack>
    </Card>)}
  </VStack>;
}
