import { proxyRPC } from "@/lib/api/proxy";
import { authorizeRPC } from "@/lib/auth/server";
import { authConfig } from "@/lib/auth/config";

export const runtime = "nodejs";

export async function POST(request: Request, context: { params: Promise<{ service: string; method: string }> }) {
  const { service, method } = await context.params;
  const authorized = await authorizeRPC(request);
  if (authorized instanceof Response) return authorized;
  const response = await proxyRPC(authorized.request, service, method, { trustedOrigin: authConfig()?.origin });
  for (const value of authorized.headers.getSetCookie()) response.headers.append("Set-Cookie", value);
  return response;
}
