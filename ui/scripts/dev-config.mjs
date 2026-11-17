import { readFileSync } from "node:fs";
import { parseEnv } from "node:util";
import { parse } from "yaml";

// Development only. Production continues to require explicit AUTH_ENABLED.
export function developmentAuth(configPath, credentialsPath, env = process.env) {
  const settings = parse(readFileSync(configPath, "utf8"));
  const auth = settings?.auth;
  if (auth != null && (typeof auth !== "object" || Array.isArray(auth))) throw new Error("auth must be a YAML mapping");
  const mode = auth?.mode || "none";
  if (!["none", "optional", "required"].includes(mode)) throw new Error("Unsupported auth.mode in local server config");
  if (mode === "none") return { AUTH_ENABLED: "false" };
  let generated = {};
  try { generated = parseEnv(readFileSync(credentialsPath, "utf8")); }
  catch (error) { if (error.code !== "ENOENT") throw error; }
  const values = { ...generated, ...env };
  const issuers = auth.issuers;
  if (!Array.isArray(issuers) || !issuers.length) throw new Error("Local OIDC needs auth.issuers");
  const issuer = issuers.find(item => item.issuer === values.KEYCLOAK_ISSUER) ?? (issuers.length === 1 ? issuers[0] : undefined);
  if (!issuer?.issuer) throw new Error("Set KEYCLOAK_ISSUER to one of the configured API issuers");
  if (values.KEYCLOAK_ISSUER && values.KEYCLOAK_ISSUER !== issuer.issuer) throw new Error("UI issuer does not match the local API issuer");
  const result = { AUTH_ENABLED: "true", KEYCLOAK_ISSUER: issuer.issuer };
  for (const key of ["KEYCLOAK_CLIENT_ID", "KEYCLOAK_CLIENT_SECRET", "AUTH_SECRET", "AUTH_URL"]) {
    if (!values[key]?.trim()) throw new Error(`Missing ${key}; run make keycloak for local credentials or configure your OIDC provider`);
    result[key] = values[key];
  }
  return result;
}
