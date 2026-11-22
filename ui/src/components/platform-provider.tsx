"use client";

import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { createBrowserClients, type BrowserClients } from "@/lib/api/client";
import type { WhoAmIResponse } from "@/gen/cloud/v1/tenant_service_pb";
import type { TenantInfo } from "@/gen/cloud/v1/tenant_directory_pb";
import { useBrowserAuth } from "./auth-boundary";

interface Session { tenantId: string; token: string; revision: number; connectionRevision: number }
interface PlatformContext {
  api: BrowserClients;
  tenantId: string;
  scopeKey: string;
  identity?: WhoAmIResponse;
  confirmedTenants: readonly TenantInfo[];
  rememberCreatedTenant: (tenant: TenantInfo) => void;
  liveUpdates: boolean;
  setLiveUpdates: (enabled: boolean) => void;
  setIdentity: (identity?: WhoAmIResponse) => void;
  connect: (tenantId: string, token?: string) => void;
  disconnect: () => void;
}
const Platform = createContext<PlatformContext | null>(null);

export function PlatformProvider({ children }: { children: ReactNode }) {
  const browserAuth = useBrowserAuth();
  const [session, setSession] = useState<Session>({ tenantId: "acme", token: "", revision: 0, connectionRevision: 0 });
  const [identity, setIdentity] = useState<WhoAmIResponse>();
  const [confirmed, setConfirmed] = useState<{ scope: string; tenants: TenantInfo[] }>();
  const [liveUpdates, setLiveUpdates] = useState(false);
  const api = useMemo(() => createBrowserClients({ baseUrl: "/api/rpc", bearerToken: browserAuth.enabled ? undefined : session.token || undefined, csrfToken: browserAuth.csrfToken }), [session, browserAuth.enabled, browserAuth.csrfToken]);
  // Tenant switches remount the console, but do not change the credential connection.
  // Never expose prior confirmations before WhoAmI verifies the same principal again.
  const confirmationScope = identity ? JSON.stringify([session.connectionRevision, identity.anonymous, identity.issuer, identity.subject, identity.username]) : undefined;
  const confirmedTenants = confirmationScope && confirmed?.scope === confirmationScope ? confirmed.tenants : [];
  const rememberCreatedTenant = useCallback((tenant: TenantInfo) => {
    if (!confirmationScope) return;
    setConfirmed((previous) => ({
      scope: confirmationScope,
      tenants: [...(previous?.scope === confirmationScope ? previous.tenants.filter((item) => item.id !== tenant.id) : []), tenant],
    }));
  }, [confirmationScope]);
  const connect = useCallback((tenantId: string, token?: string) => {
    setIdentity(undefined);
    setSession((previous) => ({ tenantId: tenantId.trim(), token: token?.trim() ?? previous.token, revision: previous.revision + 1, connectionRevision: previous.connectionRevision + (token !== undefined && token.trim() !== previous.token ? 1 : 0) }));
  }, []);
  const disconnect = useCallback(() => { setIdentity(undefined); setSession((previous) => ({ tenantId: previous.tenantId, token: "", revision: previous.revision + 1, connectionRevision: previous.connectionRevision + 1 })); }, []);
  return <Platform value={{ api, tenantId: session.tenantId, scopeKey: `${session.tenantId}:${session.revision}`, identity, confirmedTenants, rememberCreatedTenant, liveUpdates, setLiveUpdates, connect, disconnect, setIdentity }}>{children}</Platform>;
}

export function usePlatform() {
  const context = useContext(Platform);
  if (!context) throw new Error("Platform client must be used within PlatformProvider");
  return context;
}
