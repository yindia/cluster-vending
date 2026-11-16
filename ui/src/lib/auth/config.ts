export interface AuthConfig {
  enabled: true;
  origin: string;
  issuer: string;
  clientId: string;
  clientSecret: string;
  secret: string;
  secure: boolean;
  scopes: string;
}

export function authEnabled(env: Readonly<Record<string, string | undefined>> = process.env): boolean {
  if (env.AUTH_ENABLED === undefined) {
    if (env.NODE_ENV === "production") throw new Error("Set AUTH_ENABLED explicitly in production");
    return false;
  }
  if (env.AUTH_ENABLED === "false") return false;
  if (env.AUTH_ENABLED !== "true") throw new Error("AUTH_ENABLED must be true or false");
  return true;
}

function trustedURL(raw: string, production: boolean): URL {
  const url = new URL(raw);
  const loopback = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  if (url.username || url.password || url.search || url.hash || (url.protocol !== "https:" && !(url.protocol === "http:" && loopback && !production))) {
    throw new Error("Authentication URLs require HTTPS; development HTTP is limited to loopback");
  }
  return url;
}

export function authConfig(env: Readonly<Record<string, string | undefined>> = process.env): AuthConfig | undefined {
  if (!authEnabled(env)) return undefined;
  const required = (key: string) => {
    const value = env[key]?.trim();
    if (!value) throw new Error(`Authentication requires ${key}`);
    return value;
  };
  const origin = trustedURL(required("AUTH_URL"), env.NODE_ENV === "production");
  if (origin.pathname !== "/") throw new Error("AUTH_URL must be an origin without a path");
  const issuer = trustedURL(required("KEYCLOAK_ISSUER"), env.NODE_ENV === "production");
  const secret = required("AUTH_SECRET");
  if (new TextEncoder().encode(secret).length < 32) throw new Error("AUTH_SECRET needs at least 32 random bytes");
  return {
    enabled: true, origin: origin.origin, issuer: issuer.href.replace(/\/$/, ""),
    clientId: required("KEYCLOAK_CLIENT_ID"), clientSecret: required("KEYCLOAK_CLIENT_SECRET"), secret,
    secure: origin.protocol === "https:", scopes: env.KEYCLOAK_SCOPES?.trim() || "openid profile email",
  };
}

export function safeReturnTo(value: string | null, origin: string): string {
  if (!value || value.length > 2048 || !value.startsWith("/") || value.startsWith("//") || value.includes("\\")) return "/";
  try {
    const decoded = decodeURIComponent(value);
    if (decoded.startsWith("//") || decoded.startsWith("/api/") || decoded.includes("\\") || /[\r\n]/.test(decoded)) return "/";
    const url = new URL(value, origin);
    if (url.origin !== origin || url.pathname.startsWith("/api/")) return "/";
    return url.pathname + url.search + url.hash;
  } catch { return "/"; }
}
