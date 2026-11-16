import type {
  ProviderCapability,
  ProviderDefinition,
  ProviderId,
} from "./types";

/** Backend support, not a claim that the preview UI implements these workflows. */
export const providerRegistry = Object.freeze({
  aws: Object.freeze({
    id: "aws",
    label: "Amazon Web Services",
    status: "supported",
    enabled: true,
    capabilities: Object.freeze([
      "account.onboard",
      "account.verify",
      "account.delete",
      "network.create",
      "network.import",
      "network.takeover",
      "network.updatePlan",
      "network.events",
      "network.delete",
      "cluster.create",
      "cluster.import",
      "cluster.takeover",
      "cluster.updatePlan",
      "cluster.events",
      "cluster.retry",
      "cluster.deprecations",
      "cluster.recommendations",
      "cluster.kubeconfig",
      "cluster.delete",
    ] as const),
  }),
  gcp: Object.freeze({
    id: "gcp",
    label: "Google Cloud",
    status: "planned",
    enabled: false,
    capabilities: Object.freeze([] as const),
    disabledReason: "Planned: no GCP account, network or cluster backend contract is available.",
  }),
  azure: Object.freeze({
    id: "azure",
    label: "Microsoft Azure",
    status: "planned",
    enabled: false,
    capabilities: Object.freeze([] as const),
    disabledReason: "Planned: no Azure account, network or cluster backend contract is available.",
  }),
} as const satisfies Record<ProviderId, ProviderDefinition>);

export function isProviderId(value: string): value is ProviderId {
  return Object.hasOwn(providerRegistry, value);
}

/** Unknown provider values must never silently fall back to AWS. */
export function getProvider(value: string): ProviderDefinition | undefined {
  return isProviderId(value) ? providerRegistry[value] : undefined;
}

export function supportsCapability(
  value: string,
  capability: ProviderCapability,
): boolean {
  const provider = getProvider(value);
  return (
    provider?.status === "supported" &&
    provider.capabilities.includes(capability)
  );
}

/** For future form/adapter boundaries; server authorization is still mandatory. */
export function requireSupportedProvider(value: string) {
  const provider = getProvider(value);
  if (!provider) throw new Error(`Unknown provider: ${value}`);
  if (provider.status !== "supported") throw new Error(provider.disabledReason);
  return provider;
}
