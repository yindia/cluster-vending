import assert from "node:assert/strict";
import { afterEach, beforeEach, test, type TestContext } from "node:test";
import { authConfig, type AuthConfig } from "./config";
import { authorizeRPC, callback, logout, readSession, sessionResponse, writeSession, type UISession } from "./server";
import { proxyRPC } from "../api/proxy";
import * as logoutRoute from "../../app/api/auth/logout/route";

const origin = "https://console.example.test";
const issuer = "https://login.example.test/realms/test";
const envKeys = ["NODE_ENV", "AUTH_ENABLED", "AUTH_URL", "KEYCLOAK_ISSUER", "KEYCLOAK_CLIENT_ID", "KEYCLOAK_CLIENT_SECRET", "AUTH_SECRET"];
let prior: Record<string, string | undefined>;
let sequence = 0;
beforeEach(() => {
  prior = Object.fromEntries(envKeys.map(key => [key, process.env[key]]));
  Object.assign(process.env, {
    NODE_ENV: "production", AUTH_ENABLED: "true", AUTH_URL: origin, KEYCLOAK_ISSUER: issuer,
    KEYCLOAK_CLIENT_ID: `cv-ui-fixture-${++sequence}`, KEYCLOAK_CLIENT_SECRET: "fixture-client-secret",
    AUTH_SECRET: "fixture-session-encryption-key-at-least-32-bytes",
  });
});
afterEach(() => {
  for (const key of envKeys) {
    if (prior[key] === undefined) delete process.env[key];
    else process.env[key] = prior[key];
  }
});

function config(): AuthConfig {
  const value = authConfig();
  assert.ok(value);
  return value;
}
function session(overrides: Partial<UISession> = {}): UISession {
  return {
    kind: "session", sub: "fixture-subject", name: "acme-viewer", accessToken: "private-access-token",
    refreshToken: "private-refresh-token", accessExpiresAt: Math.floor(Date.now() / 1000) + 300,
    csrf: "fixture-csrf-value", ...overrides,
  };
}
function cookieHeader(headers: Headers): string {
  return headers.getSetCookie().filter(line => !/Max-Age=0(?:;|$)/.test(line)).map(line => line.split(";")[0]).join("; ");
}
async function encryptedCookie(value = session(), cfg = config()): Promise<{ cookie: string; headers: Headers }> {
  const headers = new Headers();
  await writeSession(headers, cfg, value);
  return { cookie: cookieHeader(headers), headers };
}
function rpc(cookie = "", overrides: Record<string, string> = {}): Request {
  return new Request(`${origin}/api/rpc/cloud.v1.TenantService/WhoAmI`, {
    method: "POST", body: "{}", headers: {
      Origin: origin, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json",
      Cookie: cookie, "X-CV-CSRF": "fixture-csrf-value", ...overrides,
    },
  });
}
function noNetwork(t: TestContext) {
  return t.mock.method(globalThis, "fetch", async () => { throw new Error("unexpected network request"); });
}

test("session cookies encrypt tokens, round-trip across chunks, and enforce Secure HttpOnly host scope", async () => {
  const value = session({ accessToken: "access-sensitive-" + "a".repeat(3500), refreshToken: "refresh-sensitive-" + "r".repeat(1000) });
  const { cookie, headers } = await encryptedCookie(value);
  for (const line of headers.getSetCookie()) {
    assert.match(line, /^__Host-cv\.session\.(?:count|[0-3])=/);
    assert.match(line, /; Path=\//);
    assert.match(line, /; HttpOnly/);
    assert.match(line, /; SameSite=Lax/);
    assert.match(line, /; Secure/);
    assert.doesNotMatch(line, /; Domain=/i);
  }
  const count = Number(cookie.match(/__Host-cv\.session\.count=(\d+)/)?.[1]);
  assert.ok(count > 1 && count <= 4);
  assert.doesNotMatch(cookie, /access-sensitive|refresh-sensitive|acme-viewer|fixture-csrf-value/);
  const restored = await readSession(rpc(cookie), config());
  assert.equal(restored?.accessToken, value.accessToken);
  assert.equal(restored?.refreshToken, value.refreshToken);
  assert.equal(restored?.sub, value.sub);
});

test("cookie corruption, missing chunks, duplicate names, bad counts and wrong encryption context fail closed", async () => {
  const { cookie } = await encryptedCookie();
  const broken = cookie.replace(/(__Host-cv\.session\.0=)(.)/, (_, prefix, char) => prefix + (char === "A" ? "B" : "A"));
  for (const value of [broken, cookie.replace(/__Host-cv\.session\.0=[^;]+;?\s*/, ""), cookie + "; __Host-cv.session.count=1", cookie + "; " + cookie.split(";")[0], cookie.replace(/\.count=\d+/, ".count=5"), cookie.replace(/\.count=\d+/, ".count=-1")]) {
    assert.equal(await readSession(rpc(value), config()), undefined);
  }
  assert.equal(await readSession(rpc(cookie), { ...config(), secret: "another-valid-encryption-key-at-least-32-bytes" }), undefined);
  assert.equal(await readSession(rpc(cookie), { ...config(), clientId: "another-client" }), undefined);
  assert.equal(await readSession(rpc(cookie), { ...config(), origin: "https://another.example.test" }), undefined);
});

test("expired encrypted sessions and oversized payloads are refused", async () => {
  const { cookie } = await encryptedCookie(session({ exp: Math.floor(Date.now() / 1000) - 1 }));
  assert.equal(await readSession(rpc(cookie), config()), undefined);
  const headers = new Headers();
  await assert.rejects(writeSession(headers, config(), session({ accessToken: "x".repeat(15000) })), /budget/);
  assert.equal(headers.getSetCookie().length, 0);
});

test("origin and CSRF denial happens before refresh or upstream fetch", async t => {
  const network = noNetwork(t);
  const { cookie } = await encryptedCookie(session({ accessExpiresAt: 0 }));
  const deniedHeaders: Record<string, string>[] = [{ Origin: "https://evil.test" }, { Origin: "" }, { "Sec-Fetch-Site": "cross-site" }, { "X-CV-CSRF": "wrong-csrf" }, { "X-CV-CSRF": "" }];
  for (const headers of deniedHeaders) {
    const result = await authorizeRPC(rpc(cookie, headers));
    assert.ok(result instanceof Response);
    assert.equal(result.status, 403);
    assert.equal(result.headers.get("cache-control"), "no-store");
  }
  assert.equal(network.mock.callCount(), 0);
});

test("browser Authorization cannot replace the encrypted session identity at the upstream", async t => {
  const network = noNetwork(t);
  const { cookie } = await encryptedCookie();
  const authorized = await authorizeRPC(rpc(cookie, { Authorization: "Bearer browser-platform-admin" }));
  assert.ok(!(authorized instanceof Response));
  assert.equal(authorized.request.headers.get("Authorization"), "Bearer private-access-token");
  assert.equal(authorized.request.headers.get("Cookie"), null);
  assert.equal(authorized.request.headers.get("X-CV-CSRF"), null);
  let calls = 0;
  const response = await proxyRPC(authorized.request, "cloud.v1.TenantService", "WhoAmI", {
    baseUrl: "https://api.example.test", trustedOrigin: origin,
    fetch: async (_url, init) => {
      calls++;
      assert.equal(new Headers(init?.headers).get("Authorization"), "Bearer private-access-token");
      assert.equal(new Headers(init?.headers).get("Cookie"), null);
      return Response.json({ username: "acme-viewer" });
    },
  });
  assert.equal(response.status, 200);
  assert.equal(calls, 1);
  assert.equal(network.mock.callCount(), 0);
});

test("expired access without refresh and browser bearer without session return 401 without anonymous fallback", async t => {
  const network = noNetwork(t);
  const { cookie } = await encryptedCookie(session({ accessExpiresAt: 0, refreshToken: undefined }));
  for (const request of [rpc(cookie), rpc("", { Authorization: "Bearer browser-token" })]) {
    const result = await authorizeRPC(request);
    assert.ok(result instanceof Response);
    assert.equal(result.status, 401);
    assert.equal((await result.json()).code, "unauthenticated");
  }
  assert.equal(network.mock.callCount(), 0);
});

test("Next request from another fetch realm forwards its body stream without Request constructor branding", async () => {
  const { cookie } = await encryptedCookie();
  const controller = new AbortController();
  const incoming = rpc(cookie, { Authorization: "Bearer browser-forgery" });
  // Next's request need not pass the native Request implementation's brand
  // check. Use its public request interface, including a real streaming body.
  const foreignRequest = {
    [Symbol.toStringTag]: "Request", url: incoming.url, method: incoming.method,
    headers: incoming.headers, body: incoming.body, signal: controller.signal,
  } as unknown as Request;
  assert.equal(foreignRequest instanceof Request, false);
  const result = await authorizeRPC(foreignRequest);
  assert.ok(!(result instanceof Response));
  assert.equal(result.request.url, incoming.url);
  assert.equal(result.request.method, "POST");
  assert.equal(result.request.headers.get("Authorization"), "Bearer private-access-token");
  assert.equal(result.request.headers.get("Cookie"), null);
  assert.equal(result.request.headers.get("X-CV-CSRF"), null);
  assert.equal(await result.request.text(), "{}");
  controller.abort();
  assert.equal(result.request.signal.aborted, true);
});

test("public session JSON contains only identity and CSRF data, never tokens or confidential settings", async t => {
  const network = noNetwork(t);
  const { cookie } = await encryptedCookie();
  const response = await sessionResponse(new Request(`${origin}/api/auth/session`, { headers: { Cookie: cookie } }));
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), { enabled: true, authenticated: true, username: "acme-viewer", csrfToken: "fixture-csrf-value" });
  assert.equal(response.headers.get("cache-control"), "no-store");
  const denied = await sessionResponse(new Request(`${origin}/api/auth/session`, { headers: { Cookie: cookie, "Sec-Fetch-Site": "cross-site" } }));
  assert.equal(denied.status, 403);
  assert.deepEqual(await denied.json(), { authenticated: false });
  assert.equal(network.mock.callCount(), 0);
});

test("refresh failure clears encrypted cookies and returns 401 without exposing provider errors", async t => {
  noNetwork(t);
  const { cookie } = await encryptedCookie(session({ accessExpiresAt: 0, refreshToken: `failed-refresh-${sequence}` }));
  const response = await authorizeRPC(rpc(cookie));
  assert.ok(response instanceof Response);
  assert.equal(response.status, 401);
  const body = await response.text();
  assert.match(body, /unauthenticated/);
  assert.doesNotMatch(body, /unexpected network|failed-refresh|fixture-client-secret|private-access/);
  assert.ok(response.headers.getSetCookie().length > 0);
  for (const line of response.headers.getSetCookie()) assert.match(line, /Max-Age=0/);
});

test("callback without a valid transaction clears login and session cookies before any provider call", async t => {
  const network = noNetwork(t);
  for (const cookie of ["", "__Host-cv.transaction=corrupt", "__Host-cv.transaction=a; __Host-cv.transaction=b"]) {
    const response = await callback(new Request(`${origin}/api/auth/callback/keycloak?code=untrusted&state=untrusted`, { headers: { Cookie: cookie } }));
    assert.equal(response.status, 303);
    assert.equal(response.headers.get("location"), "/?authError=signin");
    const cleared = response.headers.getSetCookie();
    assert.ok(cleared.some(line => line.startsWith("__Host-cv.transaction=")));
    assert.ok(cleared.some(line => line.startsWith("__Host-cv.session.count=")));
    for (const line of cleared) assert.match(line, /Max-Age=0/);
  }
  assert.equal(network.mock.callCount(), 0);
});

test("logout is exported only as POST and denies bad origin, missing session, CSRF and malformed forms before discovery", async t => {
  assert.equal("GET" in logoutRoute, false);
  assert.equal(logoutRoute.POST, logout);
  const network = noNetwork(t);
  const { cookie } = await encryptedCookie();
  for (const [headers, body, expected] of [
    [{ Origin: "https://evil.test", Cookie: cookie }, "csrfToken=fixture-csrf-value", 403],
    [{ Origin: origin }, "csrfToken=fixture-csrf-value", 403],
    [{ Origin: origin, Cookie: cookie }, "csrfToken=wrong", 403],
    [{ Origin: origin, Cookie: cookie, "Content-Type": "application/json" }, "{}", 400],
  ] as const) {
    const response = await logout(new Request(`${origin}/api/auth/logout`, { method: "POST", body, headers: { "Content-Type": "application/x-www-form-urlencoded", ...headers } }));
    assert.equal(response.status, expected);
    assert.equal(response.headers.getSetCookie().length, 0);
  }
  assert.equal(network.mock.callCount(), 0);
});

test("valid POST logout clears local secrets even when provider discovery is unavailable", async t => {
  const network = noNetwork(t);
  const { cookie } = await encryptedCookie();
  const response = await logout(new Request(`${origin}/api/auth/logout`, {
    method: "POST", body: new URLSearchParams({ csrfToken: "fixture-csrf-value" }), headers: { Origin: origin, Cookie: cookie },
  }));
  assert.equal(response.status, 303);
  assert.equal(response.headers.get("location"), "/");
  assert.match(response.headers.get("clear-site-data") ?? "", /cookies/);
  for (const line of response.headers.getSetCookie()) assert.match(line, /Max-Age=0/);
  assert.equal(network.mock.callCount(), 1);
});

test("parallel refresh and a late old-cookie request share one rotation within the process grace window", async t => {
  let rotations = 0;
  t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.pathname.endsWith("/.well-known/openid-configuration")) {
      return Response.json({ issuer, authorization_endpoint: `${issuer}/auth`, token_endpoint: `${issuer}/token`, jwks_uri: `${issuer}/certs` });
    }
    assert.equal(url.href, `${issuer}/token`);
    assert.equal(new URLSearchParams(String(init?.body)).get("grant_type"), "refresh_token");
    rotations++;
    return Response.json({ access_token: "rotated-access", refresh_token: "rotated-refresh", expires_in: 300, token_type: "Bearer" });
  });
  const { cookie } = await encryptedCookie(session({ accessExpiresAt: 0, refreshToken: `rotation-fixture-${sequence}` }));
  const results = await Promise.all([authorizeRPC(rpc(cookie)), authorizeRPC(rpc(cookie))]);
  results.push(await authorizeRPC(rpc(cookie)));
  for (const result of results) {
    assert.ok(!(result instanceof Response));
    assert.equal(result.request.headers.get("Authorization"), "Bearer rotated-access");
    assert.ok(result.headers.getSetCookie().length > 0);
  }
  assert.equal(rotations, 1);
});
