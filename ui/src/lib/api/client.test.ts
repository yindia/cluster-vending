import assert from "node:assert/strict";
import { test } from "node:test";

import { createBrowserClients } from "./client";

test("browser clients use the supplied endpoint and bearer token for all services", async (t) => {
  const requests: Request[] = [];
  t.mock.method(globalThis, "fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init);
    requests.push(request);
    const contentType = request.headers.get("Content-Type") ?? "application/json";
    return new Response(contentType === "application/proto" ? new Uint8Array() : "{}", {
      headers: { "Content-Type": contentType },
    });
  });

  const api = createBrowserClients({
    baseUrl: "https://api.example.test/connect",
    bearerToken: "user-access-token",
  });
  assert.equal(requests.length, 0, "constructing clients must not send requests");

  await api.tenant.listReleases({ tenantId: "acme" });
  await api.cluster.listClusters({ tenantId: "acme" });
  await api.component.listComponentDefinitions({ tenantId: "acme" });
  await api.network.listNetworks({ tenantId: "acme" });
  await api.tenant.listCloudAccounts({ tenantId: "acme" });
  await api.tenant.listTenants({});
  await api.tenant.createTenant({ tenantId: "new-team" });

  assert.deepEqual(
    requests.map((request) => new URL(request.url).pathname),
    [
      "/connect/cloud.v1.TenantService/ListReleases",
      "/connect/cloud.v1.ClusterService/ListClusters",
      "/connect/cloud.v1.ComponentService/ListComponentDefinitions",
      "/connect/cloud.v1.NetworkService/ListNetworks",
      "/connect/cloud.v1.TenantService/ListCloudAccounts",
      "/connect/cloud.v1.TenantService/ListTenants",
      "/connect/cloud.v1.TenantService/CreateTenant",
    ],
  );
  for (const request of requests) {
    assert.equal(request.headers.get("Authorization"), "Bearer user-access-token");
    assert.equal(new URL(request.url).origin, "https://api.example.test");
  }
  const createRequest = requests.at(-1)!;
  if (createRequest.headers.get("Content-Type")?.startsWith("application/json")) {
    assert.deepEqual(await createRequest.json(), { tenantId: "new-team" });
  }
});

test("token providers are refreshed per RPC and may omit authentication", async (t) => {
  const authorizations: (string | null)[] = [];
  t.mock.method(globalThis, "fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init);
    authorizations.push(request.headers.get("Authorization"));
    const contentType = request.headers.get("Content-Type") ?? "application/json";
    return new Response(contentType === "application/proto" ? new Uint8Array() : "{}", {
      headers: { "Content-Type": contentType },
    });
  });

  let token: string | undefined = "first-token";
  let reads = 0;
  const api = createBrowserClients({
    baseUrl: "https://api.example.test",
    bearerToken: async () => {
      reads++;
      return token;
    },
  });

  await api.cluster.listClusters({ tenantId: "acme" });
  token = "refreshed-token";
  await api.network.listNetworks({ tenantId: "acme" });
  token = undefined;
  await api.tenant.listCloudAccounts({ tenantId: "acme" });
  await createBrowserClients({ baseUrl: "https://api.example.test" })
    .cluster.listClusters({ tenantId: "acme" });

  assert.equal(reads, 3);
  assert.deepEqual(authorizations, ["Bearer first-token", "Bearer refreshed-token", null, null]);
});

test("a failing token provider does not send a request", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => {
    throw new Error("unexpected network request");
  });
  const api = createBrowserClients({
    baseUrl: "https://api.example.test",
    bearerToken: async () => {
      throw new Error("session expired");
    },
  });

  await assert.rejects(api.cluster.listClusters({ tenantId: "acme" }), /session expired/);
  assert.equal(fetch.mock.callCount(), 0);
});
