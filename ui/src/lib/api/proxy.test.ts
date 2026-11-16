import assert from "node:assert/strict";
import { test } from "node:test";
import { proxyRPC } from "./proxy";

function request(options: { origin?: string; body?: string; headers?: Record<string, string> } = {}) {
  return new Request("https://console.example.test/api/rpc/cloud.v1.TenantService/WhoAmI", {
    method: "POST", body: options.body ?? "{}",
    headers: { "Content-Type": "application/json", Origin: options.origin ?? "https://console.example.test", ...options.headers },
  });
}

test("Connect bridge forwards only caller authorization to a fixed allowlisted RPC", async () => {
  let called = 0;
  const fetcher: typeof fetch = async (url, init) => {
    called++;
    assert.equal(String(url), "https://platform.example.test/prefix/cloud.v1.TenantService/WhoAmI");
    const headers = new Headers(init?.headers);
    assert.equal(headers.get("authorization"), "Bearer user-token");
    assert.equal(headers.get("cookie"), null);
    assert.equal(headers.get("x-forwarded-host"), null);
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.redirect, "error");
    assert.equal(new TextDecoder().decode(init?.body as Uint8Array), "{}");
    return new Response('{"code":"permission_denied","message":"Tenant access denied"}', {
      status: 403, headers: { "content-type": "application/json", "set-cookie": "privileged=true", "access-control-allow-origin": "*" },
    });
  };
  const response = await proxyRPC(request({ headers: { Authorization: "Bearer user-token", Cookie: "session=secret", "X-Forwarded-Host": "attacker.test" } }), "cloud.v1.TenantService", "WhoAmI", { baseUrl: "https://platform.example.test/prefix", fetch: fetcher });
  assert.equal(called, 1);
  assert.equal(response.status, 403);
  assert.equal(response.headers.get("set-cookie"), null);
  assert.equal(response.headers.get("access-control-allow-origin"), null);
  assert.equal(response.headers.get("cache-control"), "no-store");
  assert.equal((await response.json()).message, "Tenant access denied");
});

test("bridge rejects cross-origin, unknown methods, oversized bodies and bad upstreams before fetch", async () => {
  let called = 0;
  const options = { baseUrl: "https://platform.example.test", fetch: (async () => { called++; throw new Error("unexpected request"); }) as typeof fetch };
  assert.equal((await proxyRPC(request({ origin: "https://attacker.test" }), "cloud.v1.TenantService", "WhoAmI", options)).status, 403);
  assert.equal((await proxyRPC(request(), "cloud.v1.TenantService", "NotAnRPC", options)).status, 404);
  assert.equal((await proxyRPC(request(), "cloud.v1.AdminService", "Freeze", options)).status, 404);
  assert.equal((await proxyRPC(request(), "cloud.v1.AdminService", "GetReleaseChannel", options)).status, 404);
  assert.equal((await proxyRPC(request({ headers: { "Content-Type": "text/plain" } }), "cloud.v1.TenantService", "WhoAmI", options)).status, 415);
  assert.equal((await proxyRPC(request({ body: "x".repeat(2 * 1024 * 1024 + 1) }), "cloud.v1.TenantService", "WhoAmI", options)).status, 413);
  assert.equal((await proxyRPC(request(), "cloud.v1.TenantService", "WhoAmI", { ...options, baseUrl: "file:///etc/passwd" })).status, 503);
  assert.equal((await proxyRPC(request(), "cloud.v1.TenantService", "WhoAmI", { ...options, baseUrl: "https://user:password@platform.test" })).status, 503);
  assert.equal(called, 0);
});

test("bridge uses structured errors without exposing backend credentials or network details", async () => {
  const response = await proxyRPC(request(), "cloud.v1.TenantService", "WhoAmI", {
    baseUrl: "http://127.0.0.1:8080",
    fetch: (async () => { throw new Error("socket refused at private-host with secret"); }) as typeof fetch,
  });
  assert.equal(response.status, 502);
  const body = await response.json();
  assert.equal(body.code, "unavailable");
  assert.doesNotMatch(body.message, /private-host|secret|127\.0\.0\.1/);
});
