"use client";

import { useId, useState, type FormEvent } from "react";
import { VStack, HStack } from "@astryxdesign/core/Layout";
import { FormLayout } from "@astryxdesign/core/FormLayout";
import { Heading, Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Button } from "@astryxdesign/core/Button";
import { Banner } from "@astryxdesign/core/Banner";
import { usePlatform } from "@/components/platform-provider";

export function ConnectionPanel({ onConnect, showHeading = true, formId, showConnectAction = true }: {
  onConnect?: () => void;
  showHeading?: boolean;
  formId?: string;
  showConnectAction?: boolean;
}) {
  const { tenantId, connect, disconnect, identity } = usePlatform();
  const inputId = useId();
  const [token, setToken] = useState("");
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    connect(tenantId, token.trim() || undefined);
    setToken(""); onConnect?.();
  }
  return <VStack gap={4} width="100%">
    {showHeading && <Heading level={2}>Workspace connection</Heading>}
    <Text color="secondary">Connect to the current tenant ({tenantId || "not selected"}) with an access token if required. Choose tenants in the top bar. Credentials stay in this tab’s memory.</Text>
    <form id={formId} method="post" onSubmit={submit} aria-label="Workspace connection" noValidate>
      <VStack gap={4}>
        <FormLayout direction="vertical">
          <TextInput id={`${inputId}-token`} label="Access token" htmlName="accessToken" type="password" value={token} onChange={setToken} width="100%" isOptional description="Leave blank to keep the current token. Local unauthenticated APIs can connect without one." autoComplete="off" hasAutoCorrect={false} hasSpellCheck={false} />
        </FormLayout>
        <HStack gap={2} wrap="wrap">
          {showConnectAction && <Button label="Connect" variant="primary" type="submit" />}
          <Button label="Clear credentials" type="button" onClick={() => { disconnect(); setToken(""); }} />
        </HStack>
      </VStack>
    </form>
    {identity && <Banner status="info" title={identity.anonymous ? "Local anonymous API session" : `Connected as ${identity.username || identity.subject}`} description={identity.anonymous ? "The API accepted this session without a token. Authorization remains controlled by the Go server." : `Issuer: ${identity.issuer || "configured identity provider"}`} />}
  </VStack>;
}
