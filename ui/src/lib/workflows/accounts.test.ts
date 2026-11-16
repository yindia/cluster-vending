import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { createBrowserClients } from "../api/client";
import {
  BootstrapTenantResponseSchema, CloudAccountSchema, CloudAccountState,
  VerifyCloudAccountResponseSchema, DeleteCloudAccountResponseSchema,
} from "../../gen/cloud/v1/tenant_service_pb";
import { NetworkSchema } from "../../gen/cloud/v1/network_pb";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { ListNetworksResponseSchema } from "../../gen/cloud/v1/network_service_pb";
import { ListClustersResponseSchema } from "../../gen/cloud/v1/cluster_service_pb";
import {
  accessAttemptKey, accountPage, accountProgressing, bootstrapAccount,
  bootstrapAccountRequest, deleteAccount, onboardingRequest, parseVerifyRegions,
  validateAccount, verifyAccount, accountBootstrapFields, accountStepErrors,
  loadAccountRelations, type AccountBootstrapFields,
} from "./accounts";

const fields: AccountBootstrapFields = {
  provider: "aws", accountName: " Production ", accountId: "012345678901", role: "cluster-vending-provisioner",
};
const account = () => create(CloudAccountSchema, {
  id: "c5a7565c-d860-445e-b076-2460f4ab7b0a", tenantId: "acme",
  provider: { case: "aws", value: { accountId: fields.accountId, role: fields.role } },
});

test("bootstrap uses the generated AWS oneof and preserves leading zeros", () => {
  const request = bootstrapAccountRequest("acme", fields);
  assert.equal(request.$typeName, "cloud.v1.BootstrapTenantRequest");
  assert.equal(request.tenantId, "acme");
  assert.equal(request.account?.accountName, "Production");
  assert.equal(request.account?.id, "");
  assert.equal(request.account?.provider.case, "aws");
  if (request.account?.provider.case === "aws") {
    assert.equal(request.account.provider.value.accountId, "012345678901");
    assert.equal(request.account.provider.value.externalIdSet, false);
  }
});

test("invalid IDs, role paths, long display names and planned providers are rejected", () => {
  assert.match(validateAccount({ ...fields, accountId: "123" }).accountId!, /12-digit/);
  assert.match(validateAccount({ ...fields, role: "path/to/role" }).role!, /without a path/);
  assert.match(validateAccount({ ...fields, accountName: "x".repeat(129) }).accountName!, /128/);
  assert.deepEqual(validateAccount({ ...fields, accountName: "", role: "custom_role+=,.@-" }), {});
  assert.throws(() => bootstrapAccountRequest("acme", { ...fields, provider: "gcp" }), /Planned/);
  assert.throws(() => bootstrapAccountRequest("", fields), /Select a tenant/);
});

test("verification regions are optional, trimmed and deduplicated", () => {
  assert.deepEqual(parseVerifyRegions(" , "), []);
  assert.deepEqual(parseVerifyRegions("us-east-1, eu-west-1,us-east-1,"), ["us-east-1", "eu-west-1"]);
  assert.throws(() => parseVerifyRegions("us east 1"), /comma-separated/);
});

test("READY is not proof of verified access; stale or unattempted checks keep progressing", () => {
  const value = account();
  value.status = create(CloudAccountSchema, { status: { state: CloudAccountState.READY } }).status;
  assert.equal(accountProgressing(value), true);
  const checked = create(CloudAccountSchema, { status: {
    state: CloudAccountState.READY,
    access: { verified: false, checkedTime: { seconds: BigInt(1) }, attemptedTime: { seconds: BigInt(1), nanos: 2 } },
  } });
  assert.equal(accountProgressing(checked), false, "a completed denied check is terminal too");
  assert.equal(accessAttemptKey(checked), "1:2");
  assert.equal(accessAttemptKey(undefined), "");
  checked.status!.access!.stale = true;
  assert.equal(accountProgressing(checked), true);
});

test("local pagination clamps after deletion and never loses account identity", () => {
  const accounts = Array.from({ length: 21 }, (_, index) => create(CloudAccountSchema, { id: String(index) }));
  assert.deepEqual(accountPage(accounts, 2).accounts.map((item) => item.id), Array.from({ length: 10 }, (_, index) => String(index + 10)));
  assert.equal(accountPage(accounts.slice(0, 10), 3).page, 1);
  assert.deepEqual(accountPage([], 8), { page: 1, accounts: [] });
});

test("onboarding is account-specific and carries server-generated external ID lookup identity", () => {
  const request = onboardingRequest("acme", account());
  assert.equal(request.id, account().id);
  assert.equal(request.accountId, fields.accountId);
  assert.equal(request.roleName, fields.role);
  assert.equal(request.role, "provisioner");
  const custom = account();
  if (custom.provider.case === "aws") custom.provider.value.role = "cluster-vending-team-reader";
  assert.equal(onboardingRequest("acme", custom).role, "reader");
  if (custom.provider.case === "aws") custom.provider.value.role = "existing-custom-role";
  assert.throws(() => onboardingRequest("acme", custom), /Configure an existing custom role separately/);
});

test("bootstrap sends the request and rejects an incomplete response without claiming success", async (t) => {
  const api = createBrowserClients({ baseUrl: "https://offline.invalid" });
  t.mock.method(globalThis, "fetch", () => { throw new Error("Network calls are forbidden in offline tests"); });
  const saved = account();
  const call = t.mock.method(api.tenant, "bootstrapTenant", async () => create(BootstrapTenantResponseSchema, { account: saved }));
  assert.equal((await bootstrapAccount(api, "acme", fields)).id, saved.id);
  assert.equal(call.mock.calls[0].arguments[0]?.tenantId, "acme");
  call.mock.mockImplementation(async () => create(BootstrapTenantResponseSchema));
  await assert.rejects(bootstrapAccount(api, "acme", fields), /Refresh the list before retrying/);
});

test("verify preserves the previous verdict while sending the selected tenant, ID and regions", async (t) => {
  const api = createBrowserClients({ baseUrl: "https://offline.invalid" });
  t.mock.method(globalThis, "fetch", () => { throw new Error("Network calls are forbidden in offline tests"); });
  const saved = account();
  const call = t.mock.method(api.tenant, "verifyCloudAccount", async () => create(VerifyCloudAccountResponseSchema, { account: saved }));
  const response = await verifyAccount(api, "acme", saved.id, "us-east-1, us-east-1");
  assert.equal(response.account?.status?.access, undefined);
  assert.deepEqual(call.mock.calls[0].arguments[0], { tenantId: "acme", id: saved.id, regions: ["us-east-1"] });
  await assert.rejects(verifyAccount(api, "", saved.id, ""), /Select a tenant/);
  assert.equal(call.mock.callCount(), 1);
});

test("delete preserves Connect dependency errors and never requests cloud-role deletion", async (t) => {
  const api = createBrowserClients({ baseUrl: "https://offline.invalid" });
  t.mock.method(globalThis, "fetch", () => { throw new Error("Network calls are forbidden in offline tests"); });
  const error = new ConnectError("networks prod belong to the account; delete them first", Code.FailedPrecondition);
  const call = t.mock.method(api.tenant, "deleteCloudAccount", async () => { throw error; });
  await assert.rejects(deleteAccount(api, "acme", account().id), (actual) => actual === error);
  assert.deepEqual(call.mock.calls[0].arguments[0], { tenantId: "acme", id: account().id });
  await assert.rejects(deleteAccount(api, "", account().id), /Select a tenant/);
  assert.equal(call.mock.callCount(), 1);
});

test("edit prefill preserves existing name, provider, cloud ID and role", () => {
  const value = account();
  value.accountName = "Existing production";
  assert.deepEqual(accountBootstrapFields(value), { ...fields, accountName: value.accountName });
  assert.equal(accountBootstrapFields().role, "cluster-vending-provisioner");
});

test("wizard validates only the visible step, then validates the complete review", () => {
  const invalid = { ...fields, accountId: "", role: "bad/role" };
  assert.deepEqual(accountStepErrors(invalid, 0), {}, "the identity step does not reject hidden AWS fields");
  assert.deepEqual(Object.keys(accountStepErrors(invalid, 1)).sort(), ["accountId", "role"]);
  assert.deepEqual(accountStepErrors(invalid, 2), validateAccount(invalid));
  assert.deepEqual(Object.keys(accountStepErrors({ ...fields, accountName: "x".repeat(129) }, 0)), ["accountName"]);
});

test("related inventory exhausts pages and joins accounts through immutable IDs", async (t) => {
  const api = createBrowserClients({ baseUrl: "https://offline.invalid" });
  t.mock.method(globalThis, "fetch", () => { throw new Error("Network calls are forbidden in offline tests"); });
  const signal = new AbortController().signal;
  const network = create(NetworkSchema, { id: "related-network", name: "shared-name", spec: { bootstrapId: account().id } });
  const unrelated = create(NetworkSchema, { id: "other-network", name: "shared-name", spec: { bootstrapId: "another-account" } });
  const cluster = create(ClusterSchema, { id: "related-cluster", spec: { network: { networkId: network.id } } });
  const otherCluster = create(ClusterSchema, { id: "other-cluster", spec: { network: { networkId: unrelated.id } } });
  const networks = t.mock.method(api.network, "listNetworks", async (...[request, options]: Parameters<typeof api.network.listNetworks>) => {
    assert.equal(request.tenantId, "acme");
    assert.equal(options?.signal, signal);
    return create(ListNetworksResponseSchema, request.pageToken ? { networks: [network] } : { networks: [unrelated], nextPageToken: "next-network" });
  });
  const clusters = t.mock.method(api.cluster, "listClusters", async (...[request, options]: Parameters<typeof api.cluster.listClusters>) => {
    assert.equal(request.tenantId, "acme");
    assert.equal(options?.signal, signal);
    return create(ListClustersResponseSchema, request.pageToken ? { clusters: [cluster] } : { clusters: [otherCluster], nextPageToken: "next-cluster" });
  });
  const related = await loadAccountRelations(api, "acme", account().id, signal);
  assert.deepEqual(related.networks.map((item) => item.id), [network.id]);
  assert.deepEqual(related.clusters.map((item) => item.id), [cluster.id]);
  assert.equal(networks.mock.callCount(), 2);
  assert.equal(clusters.mock.callCount(), 2);
});

test("all account mutations forward cancellation and reject a response arriving after cancellation", async (t) => {
  const api = createBrowserClients({ baseUrl: "https://offline.invalid" });
  t.mock.method(globalThis, "fetch", () => { throw new Error("Network calls are forbidden in offline tests"); });
  for (const kind of ["bootstrap", "verify", "delete"] as const) {
    const controller = new AbortController();
    const cancel = (options?: { signal?: AbortSignal }) => { assert.equal(options?.signal, controller.signal); controller.abort(); };
    if (kind === "bootstrap") {
      t.mock.method(api.tenant, "bootstrapTenant", async (...[, options]: Parameters<typeof api.tenant.bootstrapTenant>) => { cancel(options); return create(BootstrapTenantResponseSchema, { account: account() }); });
      await assert.rejects(bootstrapAccount(api, "acme", fields, controller.signal), { name: "AbortError" });
    } else if (kind === "verify") {
      t.mock.method(api.tenant, "verifyCloudAccount", async (...[, options]: Parameters<typeof api.tenant.verifyCloudAccount>) => { cancel(options); return create(VerifyCloudAccountResponseSchema, { account: account() }); });
      await assert.rejects(verifyAccount(api, "acme", account().id, "", controller.signal), { name: "AbortError" });
    } else {
      t.mock.method(api.tenant, "deleteCloudAccount", async (...[, options]: Parameters<typeof api.tenant.deleteCloudAccount>) => { cancel(options); return create(DeleteCloudAccountResponseSchema, { account: account() }); });
      await assert.rejects(deleteAccount(api, "acme", account().id, controller.signal), { name: "AbortError" });
    }
  }
});

test("an already-aborted scope cannot start account mutations or related discovery", async (t) => {
  const api = createBrowserClients({ baseUrl: "https://offline.invalid" });
  const fetch = t.mock.method(globalThis, "fetch", () => { throw new Error("Network calls are forbidden in offline tests"); });
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(bootstrapAccount(api, "acme", fields, controller.signal), { name: "AbortError" });
  await assert.rejects(verifyAccount(api, "acme", account().id, "", controller.signal), { name: "AbortError" });
  await assert.rejects(deleteAccount(api, "acme", account().id, controller.signal), { name: "AbortError" });
  await assert.rejects(loadAccountRelations(api, "acme", account().id, controller.signal), { name: "AbortError" });
  assert.equal(fetch.mock.callCount(), 0);
});
