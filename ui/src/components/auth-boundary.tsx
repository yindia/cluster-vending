"use client";

import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { HStack, VStack } from "@astryxdesign/core/Layout";
import { Card } from "@astryxdesign/core/Card";
import { Divider } from "@astryxdesign/core/Divider";
import { Heading, Text } from "@astryxdesign/core/Text";
import { Button } from "@astryxdesign/core/Button";
import { Banner } from "@astryxdesign/core/Banner";
import { KubernetesIcon } from "./console/provider-icons";

export interface BrowserSession { enabled: boolean; authenticated: boolean; username?: string; csrfToken?: string }
const disabled: BrowserSession = { enabled: false, authenticated: false };
const BrowserAuth = createContext<BrowserSession>(disabled);
export function useBrowserAuth() { return useContext(BrowserAuth); }

export function AuthBoundary({ enabled, children }: { enabled: boolean; children: ReactNode }) {
  const [session, setSession] = useState<BrowserSession | undefined>(enabled ? undefined : disabled);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    if (!enabled) return;
    const controller = new AbortController();
    fetch("/api/auth/session", { credentials: "same-origin", cache: "no-store", signal: controller.signal })
      .then(async response => {
        if (!response.ok) throw new Error("Sign-in status could not be checked.");
        const value = await response.json() as BrowserSession;
        // A server-side configuration error must never become anonymous mode.
        if (!value.enabled || (value.authenticated && !value.csrfToken)) throw new Error("Sign-in is not configured correctly.");
        setSession(value); setError("");
      }).catch(reason => { if (!controller.signal.aborted) setError(reason instanceof Error ? reason.message : "Sign-in is unavailable."); });
    const expired = () => setSession({ enabled: true, authenticated: false });
    window.addEventListener("cv:session-expired", expired);
    return () => { controller.abort(); window.removeEventListener("cv:session-expired", expired); };
  }, [enabled, retry]);
  if (enabled && !session?.authenticated) return <VStack minHeight="100dvh" width="100%" vAlign="center" hAlign="center" gap={6} padding={6} as="main" aria-label="Sign in">
    <HStack gap={3} vAlign="center"><KubernetesIcon size="lg" /><Text type="large" weight="semibold">Cluster Vending</Text></HStack>
    <Card width="min(100%, 28rem)" padding={8} elevation="low"><VStack gap={6}>
      <VStack gap={3}>
        <Text type="supporting" color="secondary">WORKSPACE ACCESS</Text>
        <Heading level={1}>Sign in to your workspace</Heading>
        <Text color="secondary">Manage your cloud accounts, networks, and Kubernetes clusters.</Text>
      </VStack>
      {error && <Banner status="error" title="Sign-in unavailable" description={error} />}
      {!session && !error ? <Button label="Checking your session…" width="100%" variant="primary" isLoading isDisabled /> : <Button label="Continue with SSO" width="100%" variant="primary" href={`/api/auth/login?returnTo=${encodeURIComponent(typeof window === "undefined" ? "/" : window.location.pathname + window.location.search)}`} />}
      {error && <Button label="Retry connection" variant="ghost" onClick={() => { setError(""); setRetry(value => value + 1); }} />}
      <Divider />
      <Text type="supporting" color="secondary">Use your organization account. You’ll be redirected to your company’s sign-in page.</Text>
    </VStack></Card>
    <Text type="supporting" color="secondary">Access is limited to your assigned tenants.</Text>
  </VStack>;
  return <BrowserAuth value={session ?? disabled}>{children}</BrowserAuth>;
}

export function SignOutForm() {
  const session = useBrowserAuth();
  if (!session.enabled) return null;
  return <form action="/api/auth/logout" method="post">
    <input type="hidden" name="csrfToken" value={session.csrfToken ?? ""} />
    <Button label="Sign out" type="submit" variant="secondary" />
  </form>;
}
