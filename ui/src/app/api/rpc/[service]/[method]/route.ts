import { proxyRPC } from "@/lib/api/proxy";

export const runtime = "nodejs";

export async function POST(request: Request, context: { params: Promise<{ service: string; method: string }> }) {
  const { service, method } = await context.params;
  return proxyRPC(request, service, method);
}
