import type { JsonObject } from "@bufbuild/protobuf";
import { stringify } from "yaml";
import catalog from "./aws-preview-schemas.json";
import type { AwsResourcePreview, PreviewService } from "./aws-resource-previews";

type Parameter = {
  type: string; description?: string; enum?: readonly unknown[];
  properties?: Record<string, Parameter>; required?: readonly string[];
  items?: Parameter; additionalProperties?: Parameter;
};
export const previewSchemas: Record<keyof typeof catalog.schemas, {
  kind: string; version: string; source: string; parameters: Parameter; envelope: string[]; rules: string[];
}> = catalog.schemas;
export const providerVersion = catalog.version;

export function previewDefaults(service: PreviewService): JsonObject {
  switch (service) {
    case "s3": return {};
    case "rds": return {
      engine: "postgres", allocatedStorage: 20, instanceClass: "db.t4g.micro",
      username: "dbadmin", manageMasterUserPassword: true, publiclyAccessible: false, storageEncrypted: true,
      backupRetentionPeriod: 7, multiAz: false,
    };
    case "elasticache": return {
      engine: "valkey", nodeType: "cache.t4g.micro", numCacheClusters: 1, snapshotRetentionLimit: 0,
      transitEncryptionEnabled: true, atRestEncryptionEnabled: "true",
    };
  }
}

// Accept spec.forProvider, the spec body, and the bare parameter mappings used by earlier previews.
export function additionalParameters(input: JsonObject): JsonObject {
  let section = input;
  for (const key of ["spec", "forProvider"]) {
    if (!Object.hasOwn(section, key)) continue;
    const child = section[key];
    if (Object.keys(section).length !== 1 || !child || typeof child !== "object" || Array.isArray(child)) {
      throw new Error("Use a spec.forProvider mapping for additional resource settings.");
    }
    section = child;
  }
  return section;
}

function check(schema: Parameter, value: unknown, path: string) {
  if (schema.type === "object") {
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(path + " must be an object.");
    for (const required of schema.required ?? []) {
      if (!Object.hasOwn(value, required)) throw new Error(path + "." + required + " is required.");
    }
    for (const [key, child] of Object.entries(value)) {
      const property = schema.properties && Object.hasOwn(schema.properties, key) ? schema.properties[key] : schema.additionalProperties;
      if (!property) throw new Error(path + "." + key + " is not defined by this CRD.");
      check(property, child, path + "." + key);
    }
  } else if (schema.type === "array") {
    if (!Array.isArray(value)) throw new Error(path + " must be an array.");
    if (schema.items) value.forEach((item, index) => check(schema.items!, item, path + "[" + index + "]"));
  } else if (schema.type === "integer" ? !Number.isSafeInteger(value) : typeof value !== schema.type) {
    throw new Error(path + " must be a " + schema.type + ".");
  }
  if (schema.enum && !schema.enum.includes(value)) throw new Error(path + " accepts " + schema.enum.join(", ") + ".");
}

export function validatePreviewParameters(service: PreviewService, values: JsonObject) {
  const schema = previewSchemas[service];
  check(schema.parameters, values, "spec.forProvider");
  if (typeof values.region !== "string" || !values.region.trim()) throw new Error("Provide an AWS region.");
  // These forms create resources using forProvider. The CRD also permits initProvider
  // to supply the same create-time parameters for clients that use it.
  for (const rule of schema.rules) {
    const field = /^spec\.forProvider\.(\w+) is a required parameter$/.exec(rule)?.[1];
    if (field && !values[field]) throw new Error(rule);
  }
}

export function previewYaml(preview: AwsResourcePreview, tenantId: string, clusterId: string, subtenantId?: string) {
  const schema = previewSchemas[preview.service];
  const labels = {
    "cloud.cluster-vending.io/cluster-id": clusterId,
    ...(subtenantId ? { "cloud.cluster-vending.io/subtenant-id": subtenantId } : {}),
  };
  const render = (contract: typeof schema, fields: JsonObject) => stringify({
    apiVersion: contract.version, kind: contract.kind, metadata: { name: preview.name, namespace: "tenant-" + tenantId, labels },
    spec: { providerConfigRef: { kind: "ProviderConfig", name: "<account-provider-config>" }, forProvider: fields },
  });
  if (!preview.parameters) return "";
  let yaml = render(schema, preview.parameters);
  if (preview.service !== "s3") return yaml;
  const bucket = { region: preview.parameters.region, bucketRef: { name: preview.name } };
  if (preview.versioning ?? (preview.settings?.[0] === "Enabled")) {
    yaml += "---\n" + render(previewSchemas.bucketVersioning, { ...bucket, versioningConfiguration: { status: "Enabled" } });
  }
  const controls = preview.bucketControls;
  if (controls?.blockPublicAccess) {
    yaml += "---\n" + render(previewSchemas.publicAccess, { ...bucket, blockPublicAcls: true, ignorePublicAcls: true, blockPublicPolicy: true, restrictPublicBuckets: true });
  }
  if (controls) {
    const encryption = { sseAlgorithm: controls.encryption, ...(controls.encryption === "aws:kms" && controls.kmsKeyId.trim() ? { kmsMasterKeyId: controls.kmsKeyId.trim() } : {}) };
    yaml += "---\n" + render(previewSchemas.encryption, { ...bucket, rule: [{ applyServerSideEncryptionByDefault: encryption }] });
  }
  return yaml;
}
