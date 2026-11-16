import assert from "node:assert/strict";
import { test } from "node:test";
import { authConfig, authEnabled, safeReturnTo } from "./config";

function environment(overrides: Readonly<Record<string, string | undefined>> = {}): Record<string, string | undefined> {
  return {
    NODE_ENV: "production", AUTH_ENABLED: "true", AUTH_URL: "https://console.example.test",
    KEYCLOAK_ISSUER: "https://login.example.test/realms/platform/", KEYCLOAK_CLIENT_ID: "cv-ui",
    KEYCLOAK_CLIENT_SECRET: "fixture-client-secret", AUTH_SECRET: "fixture-session-secret-at-least-32-bytes", ...overrides,
  };
}

test("production requires an explicit auth decision and enabled auth fails closed", () => {
  assert.throws(() => authEnabled({ NODE_ENV: "production" }), /AUTH_ENABLED/);
  assert.equal(authConfig({ NODE_ENV: "production", AUTH_ENABLED: "false" }), undefined);
  assert.equal(authEnabled({ NODE_ENV: "development" }), false);
  for (const value of ["", "TRUE", "1", "yes", " true "]) {
    assert.throws(() => authConfig(environment({ AUTH_ENABLED: value })), /AUTH_ENABLED/);
  }
  for (const key of ["AUTH_URL", "KEYCLOAK_ISSUER", "KEYCLOAK_CLIENT_ID", "KEYCLOAK_CLIENT_SECRET", "AUTH_SECRET"]) {
    assert.throws(() => authConfig(environment({ [key]: undefined })), new RegExp(key));
    assert.throws(() => authConfig(environment({ [key]: "   " })), new RegExp(key));
  }
  assert.throws(() => authConfig(environment({ AUTH_SECRET: "short" })), /AUTH_SECRET/);
});

test("config normalizes HTTPS origins, keeps confidential settings server-side, and defaults scopes", () => {
  const config = authConfig(environment());
  assert.ok(config);
  assert.equal(config.origin, "https://console.example.test");
  assert.equal(config.issuer, "https://login.example.test/realms/platform");
  assert.equal(config.secure, true);
  assert.equal(config.scopes, "openid profile email");
  assert.equal(config.clientId, "cv-ui");
  assert.equal(config.clientSecret, "fixture-client-secret");
});

test("HTTP is restricted to development loopback and production requires HTTPS for both URLs", () => {
  for (const host of ["localhost", "127.0.0.1", "[::1]"]) {
    const env = environment({ NODE_ENV: "development", AUTH_URL: `http://${host}:3000`, KEYCLOAK_ISSUER: `http://${host}:18080/realms/local` });
    assert.equal(authConfig(env)?.secure, false);
    assert.throws(() => authConfig({ ...env, NODE_ENV: "production" }), /HTTPS/);
  }
  assert.throws(() => authConfig(environment({ AUTH_URL: "https://console.example.test", KEYCLOAK_ISSUER: "http://localhost:18080/realms/local" })), /HTTPS/);
  for (const value of ["http://remote.example.test", "https://user:password@example.test", "https://example.test/?secret=x", "https://example.test/#fragment", "file:///tmp/auth"]) {
    for (const key of ["AUTH_URL", "KEYCLOAK_ISSUER"]) {
      assert.throws(() => authConfig(environment({ NODE_ENV: "development", [key]: value })));
    }
  }
  assert.throws(() => authConfig(environment({ AUTH_URL: "https://console.example.test/console" })), /origin/);
});

test("return redirects allow local application paths and reject external, encoded and API destinations", () => {
  const origin = "https://console.example.test";
  for (const value of [null, "", "https://evil.test", "//evil.test", "/\\evil.test", "/%2f%2fevil.test", "/%5cevil.test", "/%0d%0aLocation:evil", "/%ZZ", "/api/auth/logout", "/%61pi/auth/logout", "/%2fapi/auth/login", "/x/../api/auth/login", "/" + "x".repeat(2048)]) {
    assert.equal(safeReturnTo(value, origin), "/", `unsafe return path: ${value}`);
  }
  assert.equal(safeReturnTo("/clusters/prod?tenant=acme#status", origin), "/clusters/prod?tenant=acme#status");
  assert.equal(safeReturnTo("/accounts/acme", origin), "/accounts/acme");
});
