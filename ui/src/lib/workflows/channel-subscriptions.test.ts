import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { ClusterSchema, ReleaseChannel, UpgradeMode } from "../../gen/cloud/v1/cluster_pb";
import { NetworkSchema } from "../../gen/cloud/v1/network_pb";
import type { BrowserClients } from "../api/client";
import { clusterSubscription, loadChannelSubscriptions } from "./channel-subscriptions";

const cluster = (id: string, channel: ReleaseChannel, mode = UpgradeMode.MANUAL) => create(ClusterSchema, { id, name: id, tenantId: "acme", spec: { releaseChannel: channel, upgradeMode: mode, network: { networkId: "network-1" } } });
test("explicit subscriptions include manual clusters; only automatic unset channels default to Stable", () => {
  for (const channel of [ReleaseChannel.ALPHA, ReleaseChannel.BETA, ReleaseChannel.STABLE]) {
    assert.deepEqual(clusterSubscription(cluster("c", channel)), { channel, defaulted: false });
  }
  assert.deepEqual(clusterSubscription(cluster("c", ReleaseChannel.UNSPECIFIED, UpgradeMode.AUTOMATIC)), { channel: ReleaseChannel.STABLE, defaulted: true });
  assert.equal(clusterSubscription(cluster("c", ReleaseChannel.UNSPECIFIED)), undefined);
  assert.equal(clusterSubscription(cluster("c", ReleaseChannel.UNSPECIFIED, UpgradeMode.UNSPECIFIED)), undefined);
  assert.equal(clusterSubscription(cluster("c", 99 as ReleaseChannel, UpgradeMode.AUTOMATIC)), undefined);
});

test("subscription inventory exhausts tenant pages, deduplicates IDs and does not depend on rollout membership", async () => {
  const calls: { tenantId: string; pageToken: string }[] = [];
  const automatic = cluster("automatic", ReleaseChannel.UNSPECIFIED, UpgradeMode.AUTOMATIC);
  const manual = cluster("manual", ReleaseChannel.STABLE);
  const network = create(NetworkSchema, { id: "network-1", tenantId: "acme", name: "production" });
  const api = {
    cluster: { listClusters: async (request: { tenantId: string; pageToken: string }) => {
      calls.push(request);
      return request.pageToken ? { clusters: [automatic, manual, cluster("unassigned", ReleaseChannel.UNSPECIFIED)], nextPageToken: "" } : { clusters: [manual, cluster("beta", ReleaseChannel.BETA)], nextPageToken: "next" };
    } },
    network: { listNetworks: async () => ({ networks: [network], nextPageToken: "" }) },
  } as unknown as BrowserClients;
  const result = await loadChannelSubscriptions(api, "acme", ReleaseChannel.STABLE, new AbortController().signal);
  assert.deepEqual(result.subscribers.map(item => [item.cluster.id, item.defaulted]), [["automatic", true], ["manual", false]]);
  assert.ok(result.subscribers.every(item => item.network?.name === "production"));
  assert.deepEqual(calls.map(request => request.pageToken), ["", "next"]);
  assert.ok(calls.every(request => request.tenantId === "acme"));
});

test("network read failures retain subscriptions, while cluster failures and tenant mismatches are errors", async () => {
  const api = { cluster: { listClusters: async () => ({ clusters: [cluster("one", ReleaseChannel.ALPHA)], nextPageToken: "" }) }, network: { listNetworks: async () => { throw new Error("Network lookup unavailable"); } } } as unknown as BrowserClients;
  const signal = new AbortController().signal;
  const result = await loadChannelSubscriptions(api, "acme", ReleaseChannel.ALPHA, signal);
  assert.equal(result.subscribers.length, 1);
  assert.match(result.networkError, /Network lookup unavailable/);
  assert.equal(result.subscribers[0].network, undefined);
  await assert.rejects(loadChannelSubscriptions(api, "other", ReleaseChannel.ALPHA, signal), /selected tenant/);
  const failed = { ...api, cluster: { listClusters: async () => { throw new Error("Cluster read failed"); } } } as unknown as BrowserClients;
  await assert.rejects(loadChannelSubscriptions(failed, "acme", ReleaseChannel.ALPHA, signal), /Cluster read failed/);
});
