// Server only: this module uses Node crypto and must never enter a client bundle.
import { createHash, timingSafeEqual } from "node:crypto";
import { EncryptJWT, jwtDecrypt, type JWTPayload } from "jose";
import * as oidc from "openid-client";
import { authConfig, safeReturnTo, type AuthConfig } from "./config";

const maxChunks = 4;
const chunkSize = 3000;
const sessionLifetime = 8 * 60 * 60;
interface Transaction extends JWTPayload { kind: "transaction"; state: string; nonce: string; verifier: string; returnTo: string }
export interface UISession extends JWTPayload { kind: "session"; sub: string; name: string; accessToken: string; refreshToken?: string; accessExpiresAt: number; csrf: string }
const configurations = new Map<string, Promise<oidc.Configuration>>();

function seconds() { return Math.floor(Date.now() / 1000); }
function cookieBase(config: AuthConfig, kind: string) { return `${config.secure ? "__Host-" : ""}cv.${kind}`; }
function key(config: AuthConfig) { return createHash("sha256").update("cluster-vending-ui-session\0").update(config.secret).digest(); }

export function authHeaders(): Headers {
  return new Headers({ "Cache-Control": "no-store", Pragma: "no-cache", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff", "Cross-Origin-Resource-Policy": "same-origin" });
}
function cookie(headers: Headers, config: AuthConfig, name: string, value: string, age: number) {
  headers.append("Set-Cookie", `${name}=${value}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${Math.max(0, age)}${config.secure ? "; Secure" : ""}`);
}
function cookieJar(request: Request): Map<string, string> {
  const out = new Map<string, string>();
  for (const part of (request.headers.get("Cookie") ?? "").split(";")) {
    const at = part.indexOf("=");
    if (at < 0) continue;
    const name = part.slice(0, at).trim();
    if (out.has(name) && name.includes("cv.")) throw new Error("Ambiguous session cookies");
    out.set(name, part.slice(at + 1).trim());
  }
  return out;
}
async function seal(config: AuthConfig, payload: JWTPayload, kind: string, expires: number) {
  return new EncryptJWT({ ...payload, kind }).setProtectedHeader({ alg: "dir", enc: "A256GCM" })
    .setIssuer(config.origin).setAudience(`${config.clientId}:${kind}`).setIssuedAt().setExpirationTime(expires).encrypt(key(config));
}
async function unseal(config: AuthConfig, raw: string, kind: string): Promise<JWTPayload> {
  const { payload } = await jwtDecrypt(raw, key(config), { issuer: config.origin, audience: `${config.clientId}:${kind}`, keyManagementAlgorithms: ["dir"], contentEncryptionAlgorithms: ["A256GCM"] });
  if (payload.kind !== kind) throw new Error("Invalid session type");
  return payload;
}
export function clearAuthCookies(headers: Headers, config: AuthConfig) {
  cookie(headers, config, cookieBase(config, "transaction"), "", 0);
  cookie(headers, config, `${cookieBase(config, "session")}.count`, "", 0);
  for (let i = 0; i < maxChunks; i++) cookie(headers, config, `${cookieBase(config, "session")}.${i}`, "", 0);
}
export async function writeSession(headers: Headers, config: AuthConfig, session: UISession) {
  const expiry = session.exp ?? seconds() + sessionLifetime;
  const raw = await seal(config, session, "session", expiry);
  if (raw.length > chunkSize * maxChunks) throw new Error("Session exceeds the supported cookie budget");
  const count = Math.ceil(raw.length / chunkSize);
  for (let i = 0; i < maxChunks; i++) cookie(headers, config, `${cookieBase(config, "session")}.${i}`, raw.slice(i * chunkSize, (i + 1) * chunkSize), i < count ? expiry - seconds() : 0);
  cookie(headers, config, `${cookieBase(config, "session")}.count`, String(count), expiry - seconds());
}
export async function readSession(request: Request, config: AuthConfig): Promise<UISession | undefined> {
  try {
    const jar = cookieJar(request);
    const count = Number(jar.get(`${cookieBase(config, "session")}.count`));
    if (!Number.isInteger(count) || count < 1 || count > maxChunks) return;
    let raw = "";
    for (let i = 0; i < count; i++) {
      const piece = jar.get(`${cookieBase(config, "session")}.${i}`);
      if (!piece || piece.length > chunkSize) return;
      raw += piece;
    }
    const payload = await unseal(config, raw, "session");
    if (typeof payload.sub !== "string" || typeof payload.name !== "string" || typeof payload.accessToken !== "string" || typeof payload.accessExpiresAt !== "number" || typeof payload.csrf !== "string" || (payload.refreshToken !== undefined && typeof payload.refreshToken !== "string")) return;
    return payload as UISession;
  } catch { return; }
}

async function configuration(config: AuthConfig): Promise<oidc.Configuration> {
  // This cache contains server configuration, never end-user tokens.
  const cacheKey = createHash("sha256").update(JSON.stringify([config.issuer, config.clientId, config.clientSecret])).digest("hex");
  let pending = configurations.get(cacheKey);
  if (!pending) {
    pending = oidc.discovery(new URL(config.issuer), config.clientId, { client_secret: config.clientSecret }, oidc.ClientSecretBasic(config.clientSecret), {
      timeout: 10, ...(config.issuer.startsWith("http:") ? { execute: [oidc.allowInsecureRequests] } : {}),
    });
    configurations.set(cacheKey, pending);
    pending.catch(() => configurations.delete(cacheKey));
  }
  return pending;
}

export async function login(request: Request): Promise<Response> {
  const config = authConfig();
  if (!config) return new Response(null, { status: 303, headers: { Location: "/" } });
  const headers = authHeaders();
  try {
    const verifier = oidc.randomPKCECodeVerifier();
    const transaction: Transaction = { kind: "transaction", state: oidc.randomState(), nonce: oidc.randomNonce(), verifier, returnTo: safeReturnTo(new URL(request.url).searchParams.get("returnTo"), config.origin) };
    const url = oidc.buildAuthorizationUrl(await configuration(config), {
      redirect_uri: `${config.origin}/api/auth/callback/keycloak`, scope: config.scopes,
      response_type: "code", code_challenge_method: "S256", code_challenge: await oidc.calculatePKCECodeChallenge(verifier), state: transaction.state, nonce: transaction.nonce,
    });
    cookie(headers, config, cookieBase(config, "transaction"), await seal(config, transaction, "transaction", seconds() + 600), 600);
    headers.set("Location", url.href);
    return new Response(null, { status: 303, headers });
  } catch { return Response.json({ error: "Sign-in is temporarily unavailable. Try again." }, { status: 503, headers }); }
}

export async function callback(request: Request): Promise<Response> {
  const config = authConfig();
  if (!config) return new Response(null, { status: 303, headers: { Location: "/" } });
  const headers = authHeaders();
  cookie(headers, config, cookieBase(config, "transaction"), "", 0);
  try {
    const raw = cookieJar(request).get(cookieBase(config, "transaction"));
    if (!raw || raw.length > 4000) throw new Error("Missing login transaction");
    const transaction = await unseal(config, raw, "transaction") as Transaction;
    if (![transaction.state, transaction.nonce, transaction.verifier, transaction.returnTo].every(value => typeof value === "string" && value.length > 0)) throw new Error("Invalid login transaction");
    const url = new URL(`${config.origin}/api/auth/callback/keycloak`);
    url.search = new URL(request.url).search;
    const tokens = await oidc.authorizationCodeGrant(await configuration(config), url, { expectedState: transaction.state, expectedNonce: transaction.nonce, pkceCodeVerifier: transaction.verifier, idTokenExpected: true });
    const claims = tokens.claims();
    if (!claims?.sub || !tokens.access_token || !tokens.expires_in) throw new Error("Incomplete identity response");
    await writeSession(headers, config, { kind: "session", sub: claims.sub, name: String(claims.preferred_username ?? claims.name ?? claims.sub), accessToken: tokens.access_token, refreshToken: tokens.refresh_token, accessExpiresAt: seconds() + tokens.expires_in, csrf: oidc.randomState() });
    headers.set("Location", safeReturnTo(transaction.returnTo, config.origin));
    return new Response(null, { status: 303, headers });
  } catch {
    clearAuthCookies(headers, config);
    headers.set("Location", "/?authError=signin");
    return new Response(null, { status: 303, headers });
  }
}

// Deduplicate parallel refreshes on one instance. Deployment must use an IdP
// refresh-token reuse policy compatible with concurrent browser tabs/replicas;
// encrypted cookie sessions do not provide a distributed refresh lock.
const refreshes = new Map<string, Promise<UISession>>();
async function freshSession(config: AuthConfig, session: UISession): Promise<UISession> {
  if (session.accessExpiresAt > seconds() + 30) return session;
  if (!session.refreshToken) throw new Error("Login expired");
  const id = createHash("sha256").update(session.refreshToken).digest("hex");
  let pending = refreshes.get(id);
  if (!pending) {
    pending = (async () => {
      const token = await oidc.refreshTokenGrant(await configuration(config), session.refreshToken!);
      if (!token.access_token || !token.expires_in) throw new Error("Refresh failed");
      const claims = token.claims();
      if (claims?.sub && claims.sub !== session.sub) throw new Error("Identity changed");
      return { ...session, accessToken: token.access_token, refreshToken: token.refresh_token ?? session.refreshToken, accessExpiresAt: seconds() + token.expires_in };
    })();
    refreshes.set(id, pending);
    // Grace for requests already in flight with the old encrypted cookie.
    pending.finally(() => { const timer = setTimeout(() => refreshes.delete(id), 2000); timer.unref(); }).catch(() => {});
  }
  return pending;
}

export function sameOrigin(request: Request, config: AuthConfig): boolean {
  return request.headers.get("Origin") === config.origin && request.headers.get("Sec-Fetch-Site") !== "cross-site";
}
function sameSecret(left: string | null, right: string) {
  if (!left || Buffer.byteLength(left) !== Buffer.byteLength(right)) return false;
  return timingSafeEqual(Buffer.from(left), Buffer.from(right));
}
export async function authorizeRPC(request: Request): Promise<{ request: Request; headers: Headers } | Response> {
  const config = authConfig();
  const headers = authHeaders();
  if (!config) return { request, headers };
  // Check origin before looking at session state to avoid a cross-site oracle.
  if (!sameOrigin(request, config)) return Response.json({ code: "permission_denied", message: "Cross-origin requests are not allowed." }, { status: 403, headers });
  let session = await readSession(request, config);
  if (!session) return Response.json({ code: "unauthenticated", message: "Sign in to continue." }, { status: 401, headers });
  if (!sameSecret(request.headers.get("X-CV-CSRF"), session.csrf)) return Response.json({ code: "permission_denied", message: "Session verification failed. Reload the page." }, { status: 403, headers });
  try {
    const current = await freshSession(config, session);
    if (current !== session) await writeSession(headers, config, current);
    session = current;
  } catch {
    clearAuthCookies(headers, config);
    return Response.json({ code: "unauthenticated", message: "Your session expired. Sign in again." }, { status: 401, headers });
  }
  const forwarded = new Headers(request.headers);
  // Browser-supplied credentials can never override the authenticated session.
  forwarded.set("Authorization", `Bearer ${session.accessToken}`);
  forwarded.delete("Cookie");
  forwarded.delete("X-CV-CSRF");
  // Next's request can come from a different fetch implementation/realm. The
  // native constructor then treats it as a URL string ("[object Request]").
  // Transfer the stream explicitly so the proxy still enforces its body limit.
  const init: RequestInit & { duplex: "half" } = {
    method: request.method, headers: forwarded, body: request.body,
    signal: request.signal, duplex: "half",
  };
  return { request: new Request(request.url, init), headers };
}

export async function sessionResponse(request: Request): Promise<Response> {
  const config = authConfig();
  const headers = authHeaders();
  if (!config) return Response.json({ enabled: false, authenticated: false }, { headers });
  if (request.headers.get("Sec-Fetch-Site") === "cross-site") return Response.json({ authenticated: false }, { status: 403, headers });
  const session = await readSession(request, config);
  if (!session) return Response.json({ enabled: true, authenticated: false }, { headers });
  try {
    const fresh = await freshSession(config, session);
    if (fresh !== session) await writeSession(headers, config, fresh);
    return Response.json({ enabled: true, authenticated: true, username: fresh.name, csrfToken: fresh.csrf }, { headers });
  } catch {
    clearAuthCookies(headers, config);
    return Response.json({ enabled: true, authenticated: false }, { headers });
  }
}

export async function logout(request: Request): Promise<Response> {
  const config = authConfig();
  const headers = authHeaders();
  if (!config) return new Response(null, { status: 303, headers: { Location: "/" } });
  if (!sameOrigin(request, config)) return new Response(null, { status: 403, headers });
  const session = await readSession(request, config);
  if (!session || Number(request.headers.get("Content-Length") ?? 0) > 4096) return new Response(null, { status: 403, headers });
  let csrf: string;
  try { csrf = String((await request.formData()).get("csrfToken") ?? ""); }
  catch { return new Response(null, { status: 400, headers }); }
  if (!sameSecret(csrf, session.csrf)) return new Response(null, { status: 403, headers });
  clearAuthCookies(headers, config);
  headers.set("Clear-Site-Data", '"cookies", "storage", "cache"');
  try {
    const endpoint = (await configuration(config)).serverMetadata().end_session_endpoint;
    if (endpoint) {
      const url = new URL(endpoint);
      url.searchParams.set("client_id", config.clientId);
      url.searchParams.set("post_logout_redirect_uri", config.origin);
      headers.set("Location", url.href);
    }
  } catch { /* Local logout still completes when the IdP is unavailable. */ }
  if (!headers.has("Location")) headers.set("Location", "/");
  return new Response(null, { status: 303, headers });
}
