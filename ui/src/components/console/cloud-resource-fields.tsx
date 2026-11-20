"use client";

import { useState } from "react";
import { stringify } from "yaml";
import { Button } from "@astryxdesign/core/Button";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { Link } from "@astryxdesign/core/Link";
import { VStack } from "@astryxdesign/core/Layout";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { previewSchemas, providerVersion } from "@/lib/providers/aws-preview-parameters";
import { awsPreviewServices, type PreviewService } from "@/lib/providers/aws-resource-previews";
import { ValuesEditor } from "./values-editor";

export function CloudResourceFields({ service, value, onChange }: { service: PreviewService; value: string; onChange: (value: string) => void }) {
  const schema = previewSchemas[service];
  const example = stringify({ spec: { forProvider: awsPreviewServices[service].example } });
  const hint = example.trimEnd().split("\n").map(line => "# " + line).join("\n") + "\n";
  return <Collapsible trigger="Additional spec (YAML)" defaultIsOpen={false}><VStack gap={4} paddingBlock={3}>
    <ValuesEditor label="Additional provider spec" value={value} placeholder={hint} onChange={onChange} height="12rem"
      description="Use spec.forProvider for additional settings. YAML takes precedence over matching form fields. Review shows the merged result." />
    {!value.trim() && <Button label="Use example spec" type="button" onClick={() => onChange(example)} />}
    <Collapsible trigger={`Field reference · ${schema.kind} · ${providerVersion}`} defaultIsOpen={false}>
      <FieldReference key={service} service={service} />
    </Collapsible>
    {service === "s3" && <Link href={previewSchemas.bucketVersioning.source}>BucketVersioning CRD for object versioning</Link>}
  </VStack></Collapsible>;
}

function FieldReference({ service }: { service: PreviewService }) {
  const schema = previewSchemas[service];
  const [search, setSearch] = useState("");
  const fields = Object.entries(schema.parameters.properties ?? {});
  const matched = fields.filter(([name, field]) => (name + " " + field.description).toLowerCase().includes(search.trim().toLowerCase()));
  return <VStack gap={3}>
    <TextInput type="search" label={`Find fields in ${schema.kind}`} value={search} onChange={setSearch} hasClear />
    <Text type="supporting">{matched.length} of {fields.length} provider fields</Text>
    <VStack height="15rem" isScrollable gap={3} tabIndex={0} aria-label="Provider field reference">
      {matched.map(([name, field]) => <VStack key={name} gap={1}>
        <Text weight="medium">{name} · {field.type}{schema.parameters.required?.includes(name) || schema.rules.some(rule => rule.startsWith("spec.forProvider." + name + " ")) ? " · required on create" : ""}</Text>
        <Text type="supporting">{field.description}</Text>
      </VStack>)}
    </VStack>
    <Text type="supporting">Spec fields · {schema.envelope.join(", ")}</Text>
    <Link href={schema.source}>Full CRD with nested fields, selectors and defaults</Link>
  </VStack>;
}
