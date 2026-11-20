"use client";

import type { JsonObject } from "@bufbuild/protobuf";
import { CheckboxInput } from "@astryxdesign/core/CheckboxInput";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { TokenListInput } from "./token-list-input";
import { Grid } from "@astryxdesign/core/Grid";
import { VStack } from "@astryxdesign/core/Layout";
import { NumberInput } from "@astryxdesign/core/NumberInput";
import { SegmentedControl, SegmentedControlItem } from "@astryxdesign/core/SegmentedControl";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import type { BucketControls, PreviewService } from "@/lib/providers/aws-resource-previews";

type Props = {
  service: PreviewService;
  fields: JsonObject;
  change: (key: string, value: string | string[] | number | boolean | null) => void;
  versioning: boolean;
  setVersioning: (value: boolean) => void;
  bucket: BucketControls;
  setBucket: (value: BucketControls) => void;
};

function Choice({ label, value, options, change }: { label: string; value: string; options: Record<string, string>; change: (value: string) => void }) {
  return <VStack gap={2}><Text type="label">{label}</Text><SegmentedControl label={label} value={value} onChange={change} layout="hug">
    {Object.entries(options).map(([id, text]) => <SegmentedControlItem key={id} value={id} label={text} />)}
  </SegmentedControl></VStack>;
}

export function CloudResourceConfiguration({ service, fields, change, versioning, setVersioning, bucket, setBucket }: Props) {
  const columns = { minWidth: 210, max: 2 };
  function input(key: string, label: string, required = false) {
    return <TextInput label={label} htmlName={key} value={String(fields[key] ?? "")} isRequired={required}
      onChange={value => change(key, value)} width="100%" autoComplete="off" autoCapitalize="none" hasSpellCheck={false} />;
  }
  function number(key: string, label: string, min: number, max?: number) {
    return <NumberInput label={label} htmlName={key} value={Number(fields[key])} min={min} max={max} isIntegerOnly width="100%" onChange={value => change(key, value)} />;
  }
  function checkbox(key: string, label: string) {
    return <CheckboxInput label={label} htmlName={key} value={fields[key] === true} onChange={value => change(key, value)} />;
  }

  if (service === "s3") return <VStack gap={4}>
    <Choice label="Default encryption" value={bucket.encryption} options={{ AES256: "S3-managed key", "aws:kms": "AWS KMS" }}
      change={value => setBucket({ ...bucket, encryption: value === "aws:kms" ? "aws:kms" : "AES256" })} />
    {bucket.encryption === "aws:kms" && <TextInput label="KMS key ARN" description="Leave empty to use the AWS-managed S3 key." value={bucket.kmsKeyId} onChange={kmsKeyId => setBucket({ ...bucket, kmsKeyId })} />}
    <CheckboxInput label="Block all public access" value={bucket.blockPublicAccess} onChange={blockPublicAccess => setBucket({ ...bucket, blockPublicAccess })} />
    <CheckboxInput label="Object versioning" value={versioning} onChange={setVersioning} isDisabled={fields.objectLockEnabled === true} disabledMessage="Object Lock requires versioning." />
    <CheckboxInput label="Object Lock" description="Enable retention controls on versioned objects." value={fields.objectLockEnabled === true}
      onChange={enabled => { change("objectLockEnabled", enabled); if (enabled) setVersioning(true); }} />
  </VStack>;

  const database = service === "rds";
  const securityGroupKey = database ? "vpcSecurityGroupIds" : "securityGroupIds";
  const securityGroups = fields[securityGroupKey];
  return <VStack gap={4}>
    <Choice label={database ? "Database engine" : "Cache engine"} value={String(fields.engine)}
      options={database ? { postgres: "PostgreSQL", mysql: "MySQL" } : { valkey: "Valkey", redis: "Redis OSS" }} change={value => change("engine", value)} />
    <Grid columns={columns} gap={4}>
      {input("engineVersion", "Engine version")}
      {database ? input("instanceClass", "DB instance class", true) : input("nodeType", "Cache node type", true)}
      {database ? number("allocatedStorage", "Allocated storage (GiB)", 1) : number("numCacheClusters", "Cache nodes (including primary)", 1)}
      {database ? number("backupRetentionPeriod", "Backup retention (days)", 0, 35) : number("snapshotRetentionLimit", "Snapshot retention (days)", 0, 35)}
      {database && input("dbName", "Initial database name")}
      {database && input("username", "Master username", true)}
    </Grid>
    {database ? <>
      {checkbox("multiAz", "Multi-AZ deployment")}
      {checkbox("deletionProtection", "Deletion protection")}
    </> : <CheckboxInput label="Multi-AZ with automatic failover" value={fields.multiAzEnabled === true}
      description="Requires a primary and at least one replica." onChange={enabled => { change("multiAzEnabled", enabled); change("automaticFailoverEnabled", enabled); if (enabled && Number(fields.numCacheClusters) < 2) change("numCacheClusters", 2); }} />}
    <Collapsible trigger="Network settings" defaultIsOpen={false}><VStack gap={3} paddingBlock={3}>
      {database ? input("dbSubnetGroupName", "DB subnet group") : input("subnetGroupName", "Cache subnet group")}
      <TokenListInput label="Security group IDs" placeholder="sg-…" values={Array.isArray(securityGroups) ? securityGroups.map(String) : []} onChange={ids => change(securityGroupKey, ids.length ? ids : null)} />
    </VStack></Collapsible>
  </VStack>;
}
