import { createClient, type Client, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import { ClusterService } from "../../gen/cloud/v1/cluster_service_pb";
import { ComponentService } from "../../gen/cloud/v1/component_service_pb";
import { NetworkService } from "../../gen/cloud/v1/network_service_pb";
import { TenantService } from "../../gen/cloud/v1/tenant_service_pb";

export interface BrowserClientOptions {
  /** The caller's trusted Connect server URL, optionally including a path prefix. */
  baseUrl: string;
  csrfToken?: string;
  /** A raw access token, or a provider evaluated for each RPC to support refresh. */
  bearerToken?:
    | string
    | (() => string | undefined | Promise<string | undefined>);
}

export interface BrowserClients {
  cluster: Client<typeof ClusterService>;
  component: Client<typeof ComponentService>;
  network: Client<typeof NetworkService>;
  tenant: Client<typeof TenantService>;
}

/** Creates clients without making requests; authorization remains on the Go server. */
export function createBrowserClients({
  baseUrl,
  bearerToken,
  csrfToken,
}: BrowserClientOptions): BrowserClients {
  const authorization: Interceptor = (next) => async (request) => {
    if (csrfToken) request.header.set("X-CV-CSRF", csrfToken);
    const token =
      typeof bearerToken === "function" ? await bearerToken() : bearerToken;
    if (token) {
      request.header.set("Authorization", `Bearer ${token}`);
    }
    try { return await next(request); }
    catch (error) {
      if (csrfToken && error instanceof Error && "code" in error && error.code === 16 && typeof window !== "undefined") window.dispatchEvent(new Event("cv:session-expired"));
      throw error;
    }
  };

  const transport = createConnectTransport({
    baseUrl,
    useBinaryFormat: false,
    interceptors: [authorization],
  });

  return {
    cluster: createClient(ClusterService, transport),
    component: createClient(ComponentService, transport),
    network: createClient(NetworkService, transport),
    tenant: createClient(TenantService, transport),
  };
}
