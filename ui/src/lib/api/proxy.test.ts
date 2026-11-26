import assert from "node:assert/strict";
import { test } from "node:test";
import { checkOrigin, proxyRPC } from "./proxy";

// A browser call through a port-forward: the default login-off setup.
function request(options: { origin?: string; body?: string; headers?: Record<string, string> } = {}) {
  return new Request("http://0.0.0.0:3000/api/rpc/cloud.v1.TenantService/WhoAmI", {
    method: "POST", body: options.body ?? "{}",
    headers: { "Content-Type": "application/json", Host: "localhost:3000", Origin: options.origin ?? "http://localhost:3000", ...options.headers },
  });
}

test("bridge allowlists generated SubTenant CRUD methods", async () => {
  for (const method of ["CreateSubTenant", "GetSubTenant", "ListSubTenants", "UpdateSubTenant", "DeleteSubTenant"]) {
    let called = false;
    const response = await proxyRPC(request({ body: '{"tenantId":"acme"}' }), "cloud.v1.SubTenantService", method, {
      baseUrl: "https://platform.example.test",
      fetch: (async (url, init) => {
        called = true;
        assert.equal(String(url), `https://platform.example.test/cloud.v1.SubTenantService/${method}`);
        assert.equal(new TextDecoder().decode(init?.body as Uint8Array), '{"tenantId":"acme"}');
        return Response.json({});
      }) as typeof fetch,
    });
    assert.equal(response.status, 200);
    assert.equal(called, true);
  }
});

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

// Next's standalone server builds request.url from its listen address, not
// from the host the browser used.
function addressed(headers: Record<string, string>) {
  return new Request("http://0.0.0.0:3000/api/rpc/cloud.v1.TenantService/WhoAmI", { method: "POST", body: "{}", headers: { "Content-Type": "application/json", ...headers } });
}

test("login off: Origin must name the Host the request was addressed to, not Next's listen address", () => {
  const off = { publicOrigin: "" };
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://localhost:3000" }), off), "allow");
  assert.equal(checkOrigin(addressed({ Host: "127.0.0.1:3000", Origin: "http://127.0.0.1:3000" }), off), "allow");
  assert.equal(checkOrigin(addressed({ Host: "[::1]:3000", Origin: "http://[::1]:3000" }), off), "allow");
  assert.equal(checkOrigin(addressed({ Host: "console.localhost:3000", Origin: "http://console.localhost:3000" }), off), "allow");
  assert.equal(checkOrigin(addressed({ Host: "Localhost:3000", Origin: "http://localhost:3000" }), off), "allow");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000" }), off), "allow");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://evil.example" }), off), "deny");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://localhost:3001" }), off), "deny");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://0.0.0.0:3000" }), off), "deny");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "null" }), off), "deny");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "file://localhost:3000" }), off), "deny");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://localhost:3000/path" }), off), "deny");
  assert.equal(checkOrigin(addressed({ Origin: "http://localhost:3000" }), off), "deny");
});

test("login off: Sec-Fetch-Site cross-site is refused even when the host matches", () => {
  const off = { publicOrigin: "" };
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://localhost:3000", "Sec-Fetch-Site": "cross-site" }), off), "deny");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", "Sec-Fetch-Site": "cross-site" }), off), "deny");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://localhost:3000", "Sec-Fetch-Site": "same-origin" }), off), "allow");
});

test("login off: DNS rebinding is refused: a page whose name points at 127.0.0.1 sends its own Host", () => {
  const off = { publicOrigin: "" };
  const rebound = { Host: "attacker.example:3000", Origin: "http://attacker.example:3000", "Sec-Fetch-Site": "same-origin" };
  assert.equal(checkOrigin(addressed(rebound), off), "deny");
  assert.equal(checkOrigin(addressed({ Host: "console.example.com", Origin: "https://console.example.com" }), off), "deny");
  assert.equal(checkOrigin(addressed({ Host: "localhost.attacker.example:3000", Origin: "http://localhost.attacker.example:3000" }), off), "deny");
});

test("login off: forwarded headers never widen what passes", () => {
  const viaIngress = { Host: "console.internal:3000", "X-Forwarded-Host": "console.example.com", "X-Forwarded-Proto": "https", Origin: "https://console.example.com" };
  assert.equal(checkOrigin(addressed(viaIngress), { publicOrigin: "" }), "deny");
  assert.equal(checkOrigin(addressed({ ...viaIngress, "X-Forwarded-Host": "localhost:3000", Origin: "http://localhost:3000" }), { publicOrigin: "" }), "deny");
  assert.equal(checkOrigin(addressed(viaIngress), { publicOrigin: "https://console.example.com" }), "allow");
});

test("login off: a configured public origin is accepted in addition to the Host", () => {
  const options = { publicOrigin: "https://console.example.com/" };
  assert.equal(checkOrigin(addressed({ Host: "console.internal:3000", Origin: "https://console.example.com" }), options), "allow");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://localhost:3000" }), options), "allow");
  assert.equal(checkOrigin(addressed({ Host: "console.internal:3000", Origin: "http://console.example.com" }), options), "deny");
  assert.equal(checkOrigin(addressed({ Host: "console.internal:3000", Origin: "https://evil.example" }), options), "deny");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://localhost:3000" }), { publicOrigin: "not a url" }), "misconfigured");
});

test("login on: only the AUTH_URL origin passes, whatever the Host or forwarded headers say", () => {
  const on = { trustedOrigin: "https://console.example.com" };
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "https://console.example.com" }), on), "allow");
  assert.equal(checkOrigin(addressed({ Host: "localhost:3000", Origin: "http://localhost:3000" }), on), "deny");
  assert.equal(checkOrigin(addressed({ Host: "evil.example", "X-Forwarded-Host": "evil.example", Origin: "https://evil.example" }), on), "deny");
  assert.equal(checkOrigin(addressed({ Host: "console.example.com", Origin: "https://console.example.com", "Sec-Fetch-Site": "cross-site" }), on), "deny");
});

test("bridge forwards a same-host call behind a port-forward and refuses before fetch otherwise", async () => {
  let called = 0;
  const options = { baseUrl: "https://platform.example.test", publicOrigin: "", fetch: (async () => { called++; return Response.json({}); }) as typeof fetch };
  assert.equal((await proxyRPC(addressed({ Host: "localhost:3000", Origin: "http://localhost:3000" }), "cloud.v1.TenantService", "WhoAmI", options)).status, 200);
  assert.equal(called, 1);
  assert.equal((await proxyRPC(addressed({ Host: "localhost:3000", Origin: "http://evil.example" }), "cloud.v1.TenantService", "WhoAmI", options)).status, 403);
  assert.equal((await proxyRPC(addressed({ Host: "localhost:3000", "Sec-Fetch-Site": "cross-site" }), "cloud.v1.TenantService", "WhoAmI", options)).status, 403);
  assert.equal((await proxyRPC(addressed({ Host: "localhost:3000", Origin: "http://localhost:3000" }), "cloud.v1.TenantService", "WhoAmI", { ...options, publicOrigin: "ftp://x" })).status, 503);
  assert.equal(called, 1);
});
