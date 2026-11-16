/** UI metadata only. Wire requests/responses must use generated cloud.v1 types. */
export type ProviderId = "aws" | "gcp" | "azure";
export type SupportedProviderId = "aws";

export type ProviderCapability =
  | "account.onboard"
  | "account.verify"
  | "account.delete"
  | "network.create"
  | "network.import"
  | "network.takeover"
  | "network.updatePlan"
  | "network.events"
  | "network.delete"
  | "cluster.create"
  | "cluster.import"
  | "cluster.takeover"
  | "cluster.updatePlan"
  | "cluster.events"
  | "cluster.retry"
  | "cluster.deprecations"
  | "cluster.recommendations"
  | "cluster.kubeconfig"
  | "cluster.delete";

export type ProviderDefinition =
  | Readonly<{
      id: SupportedProviderId;
      label: string;
      status: "supported";
      enabled: true;
      capabilities: readonly ProviderCapability[];
    }>
  | Readonly<{
      id: Exclude<ProviderId, SupportedProviderId>;
      label: string;
      status: "planned";
      enabled: false;
      capabilities: readonly [];
      disabledReason: string;
    }>;

/** Provider-neutral identity for routes and selection; never an RPC payload. */
export interface AccountReference {
  readonly tenantId: string;
  /** Platform CloudAccount.id, not an AWS account number. */
  readonly id: string;
  readonly provider: ProviderId;
}

export interface NetworkReference {
  readonly tenantId: string;
  readonly id: string;
  readonly accountId: string;
  readonly provider: ProviderId;
}

export interface ClusterReference {
  readonly tenantId: string;
  readonly id: string;
  readonly accountId: string;
  readonly networkId: string;
  readonly provider: ProviderId;
}
