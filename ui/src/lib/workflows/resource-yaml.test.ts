import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { parse } from "yaml";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { NetworkSchema } from "../../gen/cloud/v1/network_pb";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { resourceYaml } from "./resource-yaml";

test("resource YAML preserves full configuration and optional false values without output-only fields", () => {
  const resource = create(ClusterSchema, { id: "cluster-id", name: "prod", tenantId: "acme", etag: "private-etag", generation: 9n, spec: { provider: { case: "aws", value: { irsa: false } }, controlPlane: { endpointAccess: { endpointPublicAccess: false } } }, status: { message: "Observed output only" } });
  const value = parse(resourceYaml({ kind: "Cluster", resource }));
  assert.deepEqual(value.metadata, { name: "prod", id: "cluster-id", tenant: "acme" });
  assert.equal(value.apiVersion, "cluster-vending.io/v1");
  assert.equal(value.spec.aws.irsa, false);
  assert.equal(value.spec.controlPlane.endpointAccess.endpointPublicAccess, false);
  assert.equal(value.status, undefined); assert.equal(value.etag, undefined); assert.equal(value.generation, undefined);
});
test("network YAML retains labels, layout and flow-log configuration", () => {
  const resource = create(NetworkSchema, { id: "network-id", name: "prod", tenantId: "acme", displayName: "Production", labels: { team: "platform" }, spec: { layout: { cidrBlock: "10.1.0.0/16" }, flowLogs: { enabled: true, retentionDays: 90 } } });
  const value = parse(resourceYaml({ kind: "Network", resource }));
  assert.equal(value.metadata.displayName, "Production"); assert.deepEqual(value.metadata.labels, { team: "platform" });
  assert.equal(value.spec.layout.cidrBlock, "10.1.0.0/16"); assert.equal(value.spec.flowLogs.retentionDays, 90);
});
test("account YAML contains only its provider configuration, without invented defaults or identity", () => {
  const resource = create(CloudAccountSchema, { id: "account-id", tenantId: "acme", accountName: "Production", provider: { case: "aws", value: { accountId: "111122223333", role: "tenant-role" } }, status: { identity: "not-configuration" } });
  const value = parse(resourceYaml({ kind: "CloudAccount", resource }));
  assert.deepEqual(value.spec, { aws: { accountId: "111122223333", role: "tenant-role" } });
  assert.equal(value.metadata.name, "Production"); assert.equal(value.status, undefined);
});
