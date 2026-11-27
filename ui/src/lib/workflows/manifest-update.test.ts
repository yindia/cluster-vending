import assert from "node:assert/strict";
import test from "node:test";
import { clone, create, fromJsonString } from "@bufbuild/protobuf";
import { AccessSpecSchema } from "../../gen/cloud/v1/access_pb";
import { ClusterSchema, UpgradeMode } from "../../gen/cloud/v1/cluster_pb";
import { FlowLogTrafficType, NetworkSchema, NetworkState } from "../../gen/cloud/v1/network_pb";
import { BootstrapTenantRequestSchema, CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { UpdateClusterRequestSchema } from "../../gen/cloud/v1/cluster_service_pb";
import { UpdateNetworkRequestSchema } from "../../gen/cloud/v1/network_service_pb";
import { PlannedAction } from "../../gen/cloud/v1/plan_pb";
import type { BrowserClients } from "../api/client";
import { applyUpdate, diffUpdate, clusterUpdatePaths, parseUpdateManifest, planApplicable, reviewUpdate, updateManifestText, type UpdateBaseline } from "./manifest-update";
import { ManifestError, serverErrorLine } from "./manifest-yaml";

const networkId = "00000000-0000-4000-8000-000000000002";
const cluster = create(ClusterSchema, {
  id: "00000000-0000-4000-8000-000000000003", tenantId: "acme", name: "payments", displayName: "Payments", etag: "cluster-etag", labels: { env: "prod" },
  spec: {
    region: "us-west-1", network: { networkId, zoneIds: ["usw1-az1", "usw1-az3"] }, upgradeMode: UpgradeMode.MANUAL,
    controlPlane: { kubernetesVersion: "1.35", endpointAccess: { endpointPublicAccess: true, endpointPrivateAccess: true, publicAccessCidrs: ["203.0.113.0/24"] }, secretsEncryption: { enabled: true } },
    provider: { case: "aws", value: { irsa: true } },
  },
});
const clusterBaseline: UpdateBaseline = { kind: "Cluster", resource: cluster, asRequested: true };
const network = create(NetworkSchema, {
  id: networkId, tenantId: "acme", name: "prod", etag: "network-etag",
  spec: { bootstrapId: "00000000-0000-4000-8000-000000000001", region: "us-west-1", layout: { cidrBlock: "10.0.0.0/16", availabilityZoneCount: 3 },
    flowLogs: { enabled: true, trafficType: FlowLogTrafficType.ALL, retentionDays: 30, aggregationIntervalSeconds: 600 } },
  status: { state: NetworkState.READY },
});
const networkBaseline: UpdateBaseline = { kind: "Network", resource: network, asRequested: true };
const account = create(CloudAccountSchema, { id: "00000000-0000-4000-8000-000000000001", tenantId: "acme", accountName: "Production", provider: { case: "aws", value: { accountId: "111122223333", role: "cluster-vending-provisioner" } } });
const accountBaseline: UpdateBaseline = { kind: "CloudAccount", resource: account };

const refusal = (text: string, baseline: UpdateBaseline) => {
  try { parseUpdateManifest(text, "acme", baseline); } catch (cause) { return cause as ManifestError; }
  return assert.fail("expected a refusal");
};
const lineOf = (text: string, needle: string) => text.split("\n").findIndex((line) => line.includes(needle)) + 1;

test("the editor opens with the object's manifest, which states nothing to change", () => {
  for (const baseline of [clusterBaseline, networkBaseline, accountBaseline]) {
    const text = updateManifestText(baseline);
    assert.match(text, /^# The /);
    assert.match(text, new RegExp(`kind: ${baseline.kind}`));
    assert.match(refusal(text, baseline).message, /Nothing to change/);
  }
});

test("only the fields the manifest changes are in the mask, sent with the etag", () => {
  const text = updateManifestText(clusterBaseline).replace("displayName: Payments", "displayName: Payments API").replace("env: prod", "env: staging");
  const parsed = parseUpdateManifest(text, "acme", clusterBaseline);
  const request = fromJsonString(UpdateClusterRequestSchema, parsed.requestJson);
  assert.deepEqual(request.updateMask?.paths, ["display_name", "labels"]);
  assert.equal(request.cluster?.etag, "cluster-etag");
  assert.equal(request.cluster?.id, cluster.id);
  assert.equal(request.validateOnly, true);
  assert.deepEqual(parsed.changes.map((change) => [change.path, change.from, change.to]), [["display_name", "Payments", "Payments API"], ["labels", '{"env":"prod"}', '{"env":"staging"}']]);
});

test("a field the manifest leaves out stays as it is, as the CLI's update -f does", () => {
  const text = updateManifestText(clusterBaseline).replace(/\n\s+endpointPrivateAccess: true/, "").replace(/\n\s+publicAccessCidrs:\n\s+- 203.0.113.0\/24/, "").replace("displayName: Payments", "displayName: Renamed");
  assert.doesNotMatch(text, /publicAccessCidrs/);
  const request = fromJsonString(UpdateClusterRequestSchema, parseUpdateManifest(text, "acme", clusterBaseline).requestJson);
  assert.deepEqual(request.updateMask?.paths, ["display_name"]);
});

test("fields set at creation are refused on the line that changes them, with why", () => {
  const base = updateManifestText(clusterBaseline);
  for (const [from, to, pattern] of [
    ["name: payments", "name: renamed", /name cannot be changed \(it names the object's resources/],
    ["region: us-west-1", "region: eu-west-1", /spec.region cannot be changed/],
    ["- usw1-az3", "- usw1-az2", /spec.network cannot be changed \(a cluster stays in the network it was created in\)/],
  ] as const) {
    const text = base.replace(from, to);
    const error = refusal(text, clusterBaseline);
    assert.match(error.message, pattern);
    assert.equal(error.line, lineOf(text, to.startsWith("- usw1") ? "network:" : to));
  }
  const other = base.replace(`id: ${cluster.id}`, "id: 00000000-0000-4000-8000-000000000099");
  assert.match(refusal(other, clusterBaseline).message, /metadata.id must stay the edited cluster's ID/);
});

test("a version under automatic upgrades is not a change; encryption cannot be turned off", () => {
  const automatic = updateManifestText(clusterBaseline).replace("upgradeMode: UPGRADE_MODE_MANUAL", "upgradeMode: UPGRADE_MODE_AUTOMATIC").replace('kubernetesVersion: "1.35"', 'kubernetesVersion: "1.36"');
  const parsed = parseUpdateManifest(automatic, "acme", clusterBaseline);
  assert.deepEqual(parsed.changes.map((change) => change.path), ["spec.upgrade_mode"]);
  assert.match(parsed.notes[0], /kubernetesVersion 1.36 is ignored/);
  const disabled = updateManifestText(clusterBaseline).replace(/secretsEncryption:\n(\s+)enabled: true/, "secretsEncryption:\n$1enabled: false");
  assert.match(refusal(disabled, clusterBaseline).message, /cannot be turned off or moved/);
});

test("clearing the admin list warns that only the platform can use kubectl", () => {
  const withAdmin = clone(ClusterSchema, cluster);
  withAdmin.spec!.access = create(AccessSpecSchema, { adminPrincipals: ["arn:aws:iam::111122223333:role/admin"] });
  const baseline: UpdateBaseline = { kind: "Cluster", resource: withAdmin, asRequested: true };
  const text = updateManifestText(baseline).replace("arn:aws:iam::111122223333:role/admin", "arn:aws:iam::111122223333:role/other");
  assert.deepEqual(parseUpdateManifest(text, "acme", baseline).changes.map((change) => change.path), ["spec.access.admin_principals"]);
});

test("network flow log settings follow the server's off-then-on rule", () => {
  const traffic = updateManifestText(networkBaseline).replace("FLOW_LOG_TRAFFIC_TYPE_ALL", "FLOW_LOG_TRAFFIC_TYPE_REJECT");
  assert.match(refusal(traffic, networkBaseline).message, /first apply enabled: false/);
  const retention = updateManifestText(networkBaseline).replace("retentionDays: 30", "retentionDays: 90").replace("TIER_UNSPECIFIED", "TIER_2");
  const request = fromJsonString(UpdateNetworkRequestSchema, parseUpdateManifest(retention, "acme", networkBaseline).requestJson);
  assert.deepEqual(request.updateMask?.paths, ["spec.flow_logs"]);
  const layout = updateManifestText(networkBaseline).replace("cidrBlock: 10.0.0.0/16", "cidrBlock: 10.9.0.0/16");
  assert.match(refusal(layout, networkBaseline).message, /spec.layout cannot be changed \(a network's layout is set when it is created\)/);
});

test("an account update keeps its AWS account ID and sends the reviewed connection", () => {
  const text = updateManifestText(accountBaseline).replace("role: cluster-vending-provisioner", "role: cluster-vending-reader");
  const parsed = parseUpdateManifest(text, "acme", accountBaseline);
  assert.deepEqual(parsed.changes, [{ path: "aws.role", from: "cluster-vending-provisioner", to: "cluster-vending-reader" }]);
  assert.equal(fromJsonString(BootstrapTenantRequestSchema, parsed.requestJson).account?.accountName, "Production");
  const moved = updateManifestText(accountBaseline).replace("111122223333", "444455556666");
  const error = refusal(moved, accountBaseline);
  assert.match(error.message, /spec.aws.accountId cannot change/);
  assert.equal(error.line, lineOf(moved, "444455556666"));
});

test("review asks the server for a plan; apply sends exactly that plan's ID", async () => {
  const calls: { validateOnly: boolean; planId: string; paths: string[] }[] = [];
  const api = {
    network: { updateNetwork: async (request: { validateOnly: boolean; planId: string; updateMask?: { paths: string[] } }) => {
      calls.push({ validateOnly: request.validateOnly, planId: request.planId, paths: request.updateMask?.paths ?? [] });
      return { updateRecorded: !request.validateOnly, plan: { planId: "plan-1", applicable: true, changes: [{ action: PlannedAction.UPDATE_IN_PLACE }] } };
    } },
  } as unknown as BrowserClients;
  const text = updateManifestText(networkBaseline).replace("name: prod\n", "name: prod\n  displayName: Production\n");
  const review = await reviewUpdate(api, text, "acme", networkBaseline, new AbortController().signal);
  assert.equal(planApplicable(review.plan), true);
  await applyUpdate(api, review, new AbortController().signal);
  assert.deepEqual(calls, [{ validateOnly: true, planId: "", paths: ["display_name"] }, { validateOnly: false, planId: "plan-1", paths: ["display_name"] }]);
});

test("a server error that names a field points at its line", () => {
  const text = updateManifestText(clusterBaseline);
  assert.equal(serverErrorLine(text, "invalid argument: spec.control_plane.kubernetes_version 1.99 is not offered"), lineOf(text, "kubernetesVersion"));
  assert.equal(serverErrorLine(text, "labels: reserved key"), lineOf(text, "labels:"));
  assert.equal(serverErrorLine(text, "the network is frozen"), undefined);
});

test("the diff compares repeated fields whole and descends only into covered messages", () => {
  const desired = create(ClusterSchema, { spec: { controlPlane: { endpointAccess: { publicAccessCidrs: ["203.0.113.0/24", "198.51.100.0/24"] } } } });
  const { changes, fixed } = diffUpdate(ClusterSchema, cluster, desired, clusterUpdatePaths);
  assert.deepEqual(changes.map((change) => change.path), ["spec.control_plane.endpoint_access.public_access_cidrs"]);
  assert.deepEqual(fixed, []);
});
