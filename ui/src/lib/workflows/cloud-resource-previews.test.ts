import assert from "node:assert/strict";
import test from "node:test";
import { parseAllDocuments } from "yaml";
import { additionalParameters, previewDefaults, previewYaml, validatePreviewParameters } from "../providers/aws-preview-parameters";
import { awsPreviewServices, readPreviews, type PreviewService } from "../providers/aws-resource-previews";

test("examples match each pinned forProvider schema and override the same top-level field", () => {
  for (const service of Object.keys(awsPreviewServices) as PreviewService[]) {
    const example = awsPreviewServices[service].example;
    const values = { ...previewDefaults(service), region: "us-west-1", ...(service === "elasticache" ? { description: "example-cache" } : {}), ...additionalParameters({ spec: { forProvider: example } }) };
    validatePreviewParameters(service, values);
    assert.deepEqual(additionalParameters({ forProvider: example }), example);
    assert.deepEqual(additionalParameters(example), example);
  }
  assert.throws(() => additionalParameters({ spec: { forProvider: {}, providerConfigRef: { name: "other" } } }), /spec.forProvider/);
  assert.throws(() => additionalParameters({ spec: null }), /spec.forProvider/);
});

test("S3 public access and KMS settings produce separate CRs in the same ownership scope", () => {
  const yaml = previewYaml({
    name: "files", service: "s3", parameters: { region: "us-west-1", objectLockEnabled: true },
    versioning: true, bucketControls: { blockPublicAccess: true, encryption: "aws:kms", kmsKeyId: "alias/example" },
  }, "acme", "cluster-1", "team-1");
  const [bucket, versioning, access, encryption] = parseAllDocuments(yaml).map(doc => doc.toJS());
  assert.equal(bucket.kind, "Bucket");
  for (const companion of [versioning, access, encryption]) {
    assert.deepEqual(companion.metadata, bucket.metadata);
    assert.deepEqual(companion.spec.forProvider.bucketRef, { name: bucket.metadata.name });
    assert.equal(companion.spec.forProvider.region, bucket.spec.forProvider.region);
  }
  assert.equal(versioning.spec.forProvider.versioningConfiguration.status, "Enabled");
  assert.equal(access.kind, "BucketPublicAccessBlock");
  assert.equal(access.spec.forProvider.blockPublicPolicy, true);
  assert.equal(encryption.kind, "BucketServerSideEncryptionConfiguration");
  assert.deepEqual(encryption.spec.forProvider.rule[0].applyServerSideEncryptionByDefault, { sseAlgorithm: "aws:kms", kmsMasterKeyId: "alias/example" });
});

test("existing tab-local previews retain their original versioning and resource set", () => {
  const old = { service: "s3", name: "archive", settings: ["Enabled"], parameters: { region: "us-west-1" } };
  const [saved] = readPreviews(JSON.stringify([old]));
  assert.deepEqual(parseAllDocuments(previewYaml(saved, "acme", "cluster-1")).map(doc => doc.toJS().kind), ["Bucket", "BucketVersioning"]);
  const withoutVersioning = { ...saved, versioning: false };
  assert.equal(parseAllDocuments(previewYaml(withoutVersioning, "acme", "cluster-1")).length, 1);
});
