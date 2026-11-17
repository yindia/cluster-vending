import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { developmentAuth } from "./dev-config.mjs";

function fixture(t, yaml) {
  const dir = mkdtempSync(join(tmpdir(), "cv-dev-auth-"));
  t.after(() => rmSync(dir, { recursive: true }));
  const config = join(dir, "config.yaml"), credentials = join(dir, "ui.env");
  writeFileSync(config, yaml);
  return { config, credentials };
}
test("commented or absent auth disables stale UI login without needing Keycloak", t => {
  const { config, credentials } = fixture(t, "# auth:\n#   mode: required\ncontext: local\n");
  assert.deepEqual(developmentAuth(config, credentials, { AUTH_ENABLED: "true" }), { AUTH_ENABLED: "false" });
});
test("uncommented auth loads private credentials and overrides stale disabled UI flag", t => {
  const { config, credentials } = fixture(t, "auth:\n  mode: required\n  issuers: [{issuer: 'http://localhost:18080/realms/cluster-vending'}]\n");
  writeFileSync(credentials, "KEYCLOAK_CLIENT_ID=cv-ui\nKEYCLOAK_CLIENT_SECRET=test-only\nAUTH_SECRET=test-only-session-secret\nAUTH_URL=http://localhost:3000\n");
  const result = developmentAuth(config, credentials, { AUTH_ENABLED: "false" });
  assert.equal(result.AUTH_ENABLED, "true");
  assert.equal(result.KEYCLOAK_CLIENT_ID, "cv-ui");
  assert.equal(result.KEYCLOAK_ISSUER, "http://localhost:18080/realms/cluster-vending");
});
test("bad or incomplete enabled auth fails closed", t => {
  const { config, credentials } = fixture(t, "auth: {mode: required, issuers: [{issuer: 'https://id.example'}]}");
  assert.throws(() => developmentAuth(config, credentials, {}), /Missing KEYCLOAK_CLIENT_ID/);
  assert.throws(() => developmentAuth(config, credentials, { KEYCLOAK_ISSUER: "https://other.example" }), /does not match/);
  writeFileSync(config, "auth: {mode: typo}");
  assert.throws(() => developmentAuth(config, credentials, {}), /Unsupported/);
  writeFileSync(config, "auth: [required]");
  assert.throws(() => developmentAuth(config, credentials, {}), /mapping/);
  writeFileSync(config, "auth: [");
  assert.throws(() => developmentAuth(config, credentials, {}));
});
