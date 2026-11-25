"use client";

import { useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Selector } from "@astryxdesign/core/Selector";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { AccessEntryTypeSchema, AccessPolicyScopeSchema } from "@/gen/cloud/v1/access_pb";
import { FormEntryRows, keyIssues, type FormEntry } from "./form-entry-rows";
import { TokenListInput } from "./token-list-input";
import { ValuesEditor } from "./values-editor";
import { compact, enumName, enumNameOptions, readList, readObject, strings, text, writeList, writeObject } from "./cluster-nested-value";

type FieldProps = { value: string; onChange: (value: string) => void; disabled: boolean };
const plain = { autoComplete: "off", autoCapitalize: "none", hasAutoCorrect: false, hasSpellCheck: false } as const;

/** Unreadable text (pasted YAML with a mistake) stays editable as text until it parses. */
function Unreadable({ label, error, value, onChange, disabled, description }: FieldProps & { label: string; error: string; description: string }) {
  return <VStack gap={3}>
    <Banner status="warning" title={`${label} could not be read`} description={`${error} Fix the text below, or clear it.`} />
    <ValuesEditor label={`${label} (YAML)`} value={value} onChange={onChange} disabled={disabled} description={description} language="yaml" />
    <HStack hAlign="end"><Button type="button" variant="ghost" label={`Clear ${label.toLowerCase()}`} onClick={() => onChange("")} isDisabled={disabled} /></HStack>
  </VStack>;
}

const oidcFields = [
  ["name", "Provider name", "Unique within the cluster, for example corp-sso."],
  ["issuerUrl", "Issuer URL", "HTTPS URL of the identity provider, for example https://login.example.com."],
  ["clientId", "Client ID", "The audience ID tokens are issued for."],
  ["usernameClaim", "Username claim", "Empty: the server default (sub)."],
  ["usernamePrefix", "Username prefix", "Prepended to usernames, for example oidc:."],
  ["groupsClaim", "Groups claim", "Claim with the user's groups, for example groups."],
  ["groupsPrefix", "Groups prefix", "Prepended to group names, for example oidc:."],
] as const;

/** spec.access.oidc: one OIDC identity provider for kubectl sign-in. */
export function OidcFields({ value, onChange, disabled }: FieldProps) {
  const parsed = readObject(value);
  const oidc = parsed.value ?? {};
  const claims = (oidc.requiredClaims ?? {}) as JsonObject;
  const [rows, setRows] = useState<FormEntry[]>(() => Object.entries(claims).map(([key, item]) => ({ id: crypto.randomUUID(), key, value: text(item), choice: "" })));
  if (parsed.error !== undefined) return <Unreadable label="OIDC provider" error={parsed.error} value={value} onChange={onChange} disabled={disabled} description="An OIDC configuration object." />;
  const enabled = !!value.trim();
  const set = (patch: JsonObject) => onChange(writeObject({ ...oidc, ...patch }) || JSON.stringify({ name: "" }));
  const entra = (oidc.entra ?? {}) as JsonObject;
  return <VStack gap={4} role="group" aria-label="OIDC identity provider">
    <CheckboxInput label="Use an OIDC identity provider" value={enabled} isDisabled={disabled} description="Lets users sign in to the Kubernetes API with tokens from your identity provider. Changing the provider replaces it."
      onChange={(on) => { setRows([]); onChange(on ? JSON.stringify({ name: "" }) : ""); }} />
    {enabled && <>
      <FormLayout>
        {oidcFields.map(([key, label, description]) => <TextInput key={key} label={label} value={text(oidc[key])} onChange={(next) => set({ [key]: next })} isDisabled={disabled}
          isRequired={key === "name" || key === "issuerUrl" || key === "clientId"} description={description} {...plain} />)}
      </FormLayout>
      <FormEntryRows label="Required claims" rowLabel="required claim" rows={rows} disabled={disabled} limit={10} issues={keyIssues(rows)}
        hint="Every token must carry these claims with these values." onChange={(next) => { setRows(next); set({ requiredClaims: Object.fromEntries(next.map((row) => [row.key, row.value])) }); }} />
      <VStack gap={3}>
        <Heading level={4}>Microsoft Entra ID</Heading>
        <Text type="supporting">Optional. Set both IDs when the issuer is Microsoft Entra ID.</Text>
        <FormLayout direction="horizontal" data-workflow-fields="">
          <TextInput label="Entra tenant ID" value={text(entra.tenantId)} onChange={(next) => set({ entra: compact({ ...entra, tenantId: next }) })} isDisabled={disabled} placeholder="UUID" {...plain} />
          <TextInput label="Entra client ID" value={text(entra.clientId)} onChange={(next) => set({ entra: compact({ ...entra, clientId: next }) })} isDisabled={disabled} placeholder="UUID" {...plain} />
        </FormLayout>
      </VStack>
    </>}
  </VStack>;
}

const policyExample = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy";

/** spec.access.access_entries: IAM principals and the EKS access policies they get. */
export function AccessEntriesFields({ value, onChange, disabled }: FieldProps) {
  const parsed = readList(value);
  if (parsed.error !== undefined) return <Unreadable label="Access entries" error={parsed.error} value={value} onChange={onChange} disabled={disabled} description="A list of access entry objects." />;
  const entries = parsed.value;
  const write = (next: JsonObject[]) => onChange(writeList(next));
  const patch = (index: number, change: JsonObject) => write(entries.map((entry, position) => position === index ? { ...entry, ...change } : entry));
  const policiesOf = (entry: JsonObject) => (Array.isArray(entry.policies) ? entry.policies : []) as JsonObject[];
  const patchPolicy = (index: number, policyIndex: number, change: JsonObject) =>
    patch(index, { policies: policiesOf(entries[index]).map((policy, position) => position === policyIndex ? compact({ ...policy, ...change }) : policy) });
  return <VStack gap={3} role="group" aria-label="Access entries">
    <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
      <VStack gap={1}><Heading level={4}>Access entries</Heading><Text type="supporting">IAM roles or users and the EKS access policies they get. A principal is either an administrator or an access entry.</Text></VStack>
      <Button type="button" variant="secondary" label="Add access entry" isDisabled={disabled || entries.length >= 50} onClick={() => write([...entries, { principalArn: "" }])} />
    </HStack>
    {!entries.length && <Text type="supporting">No access entries configured.</Text>}
    {entries.map((entry, index) => <Card key={index} padding={3}>
      <VStack gap={3} role="group" aria-label={`Access entry ${index + 1}`}>
        <HStack gap={3} hAlign="between" vAlign="center" wrap="wrap" width="100%">
          <Text type="label">Access entry {index + 1}</Text>
          <Button type="button" variant="ghost" label={`Remove access entry ${index + 1}`} onClick={() => write(entries.filter((_, position) => position !== index))} isDisabled={disabled} />
        </HStack>
        <FormLayout direction="horizontal" data-workflow-fields="">
          <TextInput label={`Access entry ${index + 1} principal ARN`} value={text(entry.principalArn)} onChange={(next) => patch(index, { principalArn: next })} isRequired isDisabled={disabled} placeholder="arn:aws:iam::111122223333:role/developers" {...plain} />
          <Selector label={`Access entry ${index + 1} type`} value={enumName(AccessEntryTypeSchema, entry.type)} options={enumNameOptions(AccessEntryTypeSchema, "Standard · server default")} isDisabled={disabled}
            onChange={(next) => patch(index, { type: next })} />
        </FormLayout>
        {policiesOf(entry).map((policy, policyIndex) => {
          const name = `Access entry ${index + 1} policy ${policyIndex + 1}`;
          const scope = enumName(AccessPolicyScopeSchema, policy.scope);
          return <VStack key={policyIndex} gap={2} role="group" aria-label={name}>
            <FormLayout direction="horizontal" data-workflow-fields="">
              <TextInput label={`${name} ARN`} value={text(policy.policyArn)} onChange={(next) => patchPolicy(index, policyIndex, { policyArn: next })} isRequired isDisabled={disabled} placeholder={policyExample} {...plain} />
              <Selector label={`${name} scope`} value={scope} options={enumNameOptions(AccessPolicyScopeSchema, "Cluster · server default")} isDisabled={disabled} onChange={(next) => patchPolicy(index, policyIndex, { scope: next })} />
            </FormLayout>
            {scope === "ACCESS_POLICY_SCOPE_NAMESPACE" && <TokenListInput label={`${name} namespaces`} values={strings(policy.namespaces)} onChange={(namespaces) => patchPolicy(index, policyIndex, { namespaces })} disabled={disabled} placeholder="payments" description="The namespaces the policy applies in. Type a value and press Enter." maxEntries={20} />}
            <HStack hAlign="end"><Button type="button" variant="ghost" label={`Remove ${name.toLowerCase()}`} isDisabled={disabled} onClick={() => patch(index, { policies: policiesOf(entry).filter((_, position) => position !== policyIndex) })} /></HStack>
          </VStack>;
        })}
        <HStack><Button type="button" variant="secondary" label={`Add policy to access entry ${index + 1}`} isDisabled={disabled || policiesOf(entry).length >= 20} onClick={() => patch(index, { policies: [...policiesOf(entry), { policyArn: "" }] })} /></HStack>
      </VStack>
    </Card>)}
  </VStack>;
}
