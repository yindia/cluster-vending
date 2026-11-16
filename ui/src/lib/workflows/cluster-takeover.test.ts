import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { ClusterSchema, ClusterState } from "../../gen/cloud/v1/cluster_pb";
import { NetworkSchema, NetworkState } from "../../gen/cloud/v1/network_pb";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { Disposition, ImportPhase } from "../../gen/cloud/v1/import_pb";
import type { BrowserClients } from "../api/client";
import { clusterTakeoverRequest, reviewClusterTakeover } from "./cluster-takeover";

function fixture() {
  const cluster = create(ClusterSchema, { id: "cluster", tenantId: "acme", name: "production", etag: "c1", generation: 2n,
    spec: { region: "us-west-1", network: { networkId: "network" }, provider: { case: "aws", value: {} } },
    status: { state: ClusterState.IMPORTED, observedGeneration: 2n, import: { phase: ImportPhase.PLANNED, resources: [{ kind: "NodeGroup", disposition: Disposition.NOT_IMPORTED }] } } });
  const network = create(NetworkSchema, { id: "network", tenantId: "acme", etag: "n1", spec: { bootstrapId: "account", region: "us-west-1" }, status: { state: NetworkState.READY } });
  const account = create(CloudAccountSchema, { id: "account", tenantId: "acme", provider: { case: "aws", value: {} } });
  const api = { cluster: { getCluster: async () => ({ cluster }) }, network: { getNetwork: async () => ({ network }) }, tenant: { getCloudAccount: async () => ({ account }) } } as unknown as BrowserClients;
  const read = () => reviewClusterTakeover(api, "acme", "cluster", new AbortController().signal);
  return { cluster, network, account, read };
}

test("takeover requires typed consent and acknowledgment of the complete plan", async () => {
  const review = await fixture().read();
  assert.throws(() => clusterTakeoverRequest(review, "production", true, false), /Type take over/);
  assert.throws(() => clusterTakeoverRequest(review, "take over production", false, false), /NOT_IMPORTED/);
  const request = clusterTakeoverRequest(review, "take over production", true, true);
  assert.equal(request.etag, "c1");
  assert.equal(request.tenantId, "acme");
  assert.equal(request.skipPlatformComponents, true);
});

test("takeover rejects discovery, stale observation, and blocking plans", async () => {
  for (const change of [
    (f: ReturnType<typeof fixture>) => { f.cluster.status!.import!.phase = ImportPhase.DISCOVERING; },
    (f: ReturnType<typeof fixture>) => { f.cluster.status!.observedGeneration = 1n; },
    (f: ReturnType<typeof fixture>) => { f.cluster.status!.import!.resources[0].disposition = Disposition.BLOCKING; },
  ]) {
    const f = fixture(); change(f); await assert.rejects(f.read);
  }
});

test("takeover rejects foreign resources and mismatched parent region", async () => {
  for (const change of [
    (f: ReturnType<typeof fixture>) => { f.cluster.tenantId = "other"; },
    (f: ReturnType<typeof fixture>) => { f.network.tenantId = "other"; },
    (f: ReturnType<typeof fixture>) => { f.account.tenantId = "other"; },
    (f: ReturnType<typeof fixture>) => { f.network.spec!.region = "us-east-1"; },
  ]) {
    const f = fixture(); change(f); await assert.rejects(f.read);
  }
});

test("takeover requires an owned network but permits provisioning after network takeover", async () => {
  const f = fixture();
  f.network.status!.state = NetworkState.IMPORTED;
  await assert.rejects(f.read, /parent network first/);
  f.network.status!.state = NetworkState.PROVISIONING;
  await f.read();
});

test("review key changes for a changed etag, plan, or parent revision", async () => {
  const f = fixture();
  const first = await f.read();
  f.cluster.etag = "c2";
  const second = await f.read();
  assert.notEqual(first.key, second.key);
  f.cluster.status!.import!.resources[0].detail = "replacement impact changed";
  const third = await f.read();
  assert.notEqual(second.key, third.key);
  f.network.etag = "n2";
  assert.notEqual(third.key, (await f.read()).key);
});
