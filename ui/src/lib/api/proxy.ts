import { TenantService } from "@/gen/cloud/v1/tenant_service_pb";
import { NetworkService } from "@/gen/cloud/v1/network_service_pb";
import { ClusterService } from "@/gen/cloud/v1/cluster_service_pb";
import { ComponentService } from "@/gen/cloud/v1/component_service_pb";
import { SubTenantService } from "@/gen/cloud/v1/subtenant_service_pb";

const services = [TenantService, NetworkService, ClusterService, ComponentService, SubTenantService];
const routes = new Set(services.flatMap((service) => service.methods.map((method) => `${service.typeName}/${method.name}`)));
const maxBodyBytes = 2 * 1024 * 1024;

function failure(status: number, code: string, message: string) {
  return Response.json({ code, message }, { status, headers: { "Cache-Control": "no-store" } });
}

export type OriginOptions = {
  /** Login on: the console's AUTH_URL origin, the only Origin accepted. */
  trustedOrigin?: string;
  /** Login off: the one non-loopback origin accepted (an Ingress host). Default: env CV_PUBLIC_ORIGIN. */
  publicOrigin?: string;
};

type OriginDecision = "allow" | "deny" | "misconfigured";

function parseOrigin(value: string): string | undefined {
  try {
    const url = new URL(value);
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password) return undefined;
    return url.origin;
  } catch { return undefined; }
}

/** Hosts a browser can only reach on the caller's own machine. */
function isLoopbackHost(hostname: string): boolean {
  const host = hostname.toLowerCase();
  return host === "localhost" || host.endsWith(".localhost") || host === "127.0.0.1" || host === "[::1]";
}

/**
 * Whether a browser Origin may call the bridge. With login on, only the
 * configured AUTH_URL origin passes (unchanged). With login off there is no
 * session to protect, so the guard is about which page may drive the API:
 * the Origin must name the Host the request was addressed to AND that host
 * must be a loopback name (localhost, *.localhost, 127.0.0.1, [::1], as with a
 * port-forward), or the Origin must equal CV_PUBLIC_ORIGIN exactly. A page
 * whose own name points at 127.0.0.1 (DNS rebinding) sends its own name as
 * both Host and Origin, which is not loopback, so it is refused. Forwarded
 * headers are never consulted. request.url is not used: Next's standalone
 * server builds it from its listen address (http://0.0.0.0:3000).
 */
export function checkOrigin(request: Request, options: OriginOptions = {}): OriginDecision {
  const origin = request.headers.get("Origin");
  if (request.headers.get("Sec-Fetch-Site") === "cross-site") return "deny";
  if (!origin) return "allow";
  if (options.trustedOrigin) return origin === options.trustedOrigin ? "allow" : "deny";
  const configured = options.publicOrigin ?? process.env.CV_PUBLIC_ORIGIN?.trim();
  if (configured) {
    const allowed = parseOrigin(configured);
    if (!allowed) return "misconfigured";
    if (origin === allowed) return "allow";
  }
  const parsed = parseOrigin(origin);
  if (!parsed || parsed !== origin) return "deny";
  const host = request.headers.get("Host")?.trim().toLowerCase();
  const url = new URL(origin);
  return host && url.host === host && isLoopbackHost(url.hostname) ? "allow" : "deny";
}

/** Fixed-upstream, allowlisted Connect bridge. No cookies or shared identity. */
export async function proxyRPC(request: Request, service: string, method: string, options: OriginOptions & {
  baseUrl?: string;
  fetch?: typeof fetch;
} = {}): Promise<Response> {
  if (request.method !== "POST") return failure(405, "unimplemented", "Only Connect POST requests are supported.");
  if (!routes.has(`${service}/${method}`)) return failure(404, "unimplemented", "Unknown platform RPC.");
  const decision = checkOrigin(request, options);
  if (decision === "misconfigured") return failure(503, "unavailable", "The console's public origin (CV_PUBLIC_ORIGIN) is not configured correctly.");
  if (decision === "deny") return failure(403, "permission_denied", "Cross-origin API requests are not allowed.");
  const contentType = request.headers.get("Content-Type") ?? "";
  if (!/^application\/(json|proto)(;|$)/i.test(contentType)) return failure(415, "invalid_argument", "Use Connect JSON or protobuf requests.");
  const length = Number(request.headers.get("Content-Length") ?? 0);
  if (length > maxBodyBytes) return failure(413, "resource_exhausted", "The request body is too large.");
  let upstream: URL;
  try {
    upstream = new URL(options.baseUrl ?? process.env.CV_API_BASE_URL ?? "http://127.0.0.1:8080");
    if (!["http:", "https:"].includes(upstream.protocol) || upstream.username || upstream.password || upstream.search || upstream.hash) throw new Error("Invalid upstream");
    upstream.pathname = `${upstream.pathname.replace(/\/$/, "")}/${service}/${method}`;
  } catch { return failure(503, "unavailable", "The platform API URL is not configured correctly."); }
  try {
    const reader = request.body?.getReader();
    const chunks: Uint8Array[] = [];
    let size = 0;
    if (reader) {
      try {
        for (;;) {
          const part = await reader.read();
          if (part.done) break;
          size += part.value.byteLength;
          if (size > maxBodyBytes) { await reader.cancel(); return failure(413, "resource_exhausted", "The request body is too large."); }
          chunks.push(part.value);
        }
      } finally { reader.releaseLock(); }
    }
    const body = new Uint8Array(size);
    let offset = 0;
    for (const chunk of chunks) { body.set(chunk, offset); offset += chunk.length; }
    const headers = new Headers({ "Content-Type": contentType });
    for (const name of ["Authorization", "Connect-Protocol-Version", "Connect-Timeout-Ms", "Accept"]) {
      const value = request.headers.get(name);
      if (value) headers.set(name, value);
    }
    const response = await (options.fetch ?? fetch)(upstream, {
      method: "POST", body, headers, cache: "no-store", credentials: "omit", redirect: "error",
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(60_000)]),
    });
    const responseHeaders = new Headers({ "Cache-Control": "no-store" });
    for (const name of ["Content-Type", "WWW-Authenticate", "Retry-After"]) {
      const value = response.headers.get(name);
      if (value) responseHeaders.set(name, value);
    }
    return new Response(response.body, { status: response.status, headers: responseHeaders });
  } catch (error) {
    if (request.signal.aborted) return failure(499, "canceled", "The request was canceled.");
    if (error instanceof DOMException && error.name === "TimeoutError") return failure(504, "deadline_exceeded", "The platform API request timed out.");
    return failure(502, "unavailable", "The platform API could not be reached. Check the server connection.");
  }
}
