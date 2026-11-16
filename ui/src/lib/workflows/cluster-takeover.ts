import { create, toJsonString } from "@bufbuild/protobuf";
import type { BrowserClients } from "../api/client";
import { supportsCapability } from "../providers/registry";
import { ClusterState, type Cluster } from "../../gen/cloud/v1/cluster_pb";
import { NetworkState, type Network } from "../../gen/cloud/v1/network_pb";
import { Disposition, ImportPhase, ImportStatusSchema } from "../../gen/cloud/v1/import_pb";
import { TakeOverClusterRequestSchema } from "../../gen/cloud/v1/cluster_service_pb";

export type ClusterTakeoverReview = { cluster: Cluster; network: Network; key: string };

/** Read the exact stored plan and its tenant-owned parent before consent. */
export async function reviewClusterTakeover(api: BrowserClients, tenantId: string, clusterId: string, signal: AbortSignal): Promise<ClusterTakeoverReview> {
  const { cluster } = await api.cluster.getCluster({ tenantId, clusterId }, { signal });
  signal.throwIfAborted();
  if (!cluster || cluster.id !== clusterId || cluster.tenantId !== tenantId || !cluster.etag) throw new Error("Reload the cluster in the active tenant to obtain its ID and etag.");
  if (!supportsCapability(cluster.spec?.provider.case ?? "", "cluster.takeover")) throw new Error("This cluster provider does not support takeover.");
  const status = cluster.status;
  if (status?.state !== ClusterState.IMPORTED || status.import?.phase !== ImportPhase.PLANNED) throw new Error("Takeover requires an IMPORTED cluster with a PLANNED import. Wait for discovery or resolve its failures.");
  // AppliedGeneration is not exposed by the API; require the observed generation
  // as a conservative readiness check. Server admission also checks application.
  if (status.observedGeneration < cluster.generation) throw new Error("Wait until the imported resources have been observed at the current generation.");
  const allowed = [Disposition.MATCHES, Disposition.WILL_UPDATE, Disposition.WILL_CREATE, Disposition.NOT_IMPORTED, Disposition.CARRIED_OVER];
  if (status.import.resources.some(resource => !allowed.includes(resource.disposition))) throw new Error("Resolve blocking or unspecified resources in the import plan before takeover.");
  const networkId = cluster.spec?.network?.networkId;
  if (!networkId) throw new Error("The cluster must reference an existing network.");
  const { network } = await api.network.getNetwork({ tenantId, networkId }, { signal });
  signal.throwIfAborted();
  if (!network || network.id !== networkId || network.tenantId !== tenantId || !network.etag) throw new Error("The parent network must belong to the active tenant and have a current etag.");
  const state = network.status?.state;
  // Takeover permits an owned network still provisioning after its own takeover;
  // unlike cluster creation, it does not require READY or UPDATING exclusively.
  if (state === undefined || state === NetworkState.UNSPECIFIED || [NetworkState.IMPORTED, NetworkState.FAILED, NetworkState.DELETING].includes(state)) throw new Error("Take over the parent network first and resolve its failures. Clusters require a network the platform owns.");
  if (network.spec?.region !== cluster.spec?.region) throw new Error("The cluster and parent network must have the same region.");
  const { account } = await api.tenant.getCloudAccount({ tenantId, id: network.spec?.bootstrapId ?? "" }, { signal });
  signal.throwIfAborted();
  if (!account || account.tenantId !== tenantId || account.id !== network.spec?.bootstrapId || account.provider.case !== cluster.spec?.provider.case) throw new Error("The parent network needs an account in this tenant with the same supported provider.");
  return { cluster, network, key: JSON.stringify([tenantId, cluster.id, cluster.name, cluster.etag, String(cluster.generation), toJsonString(ImportStatusSchema, status.import), network.id, network.etag, state, network.spec?.region, account.id, account.provider.case]) };
}

export function clusterTakeoverRequest(review: ClusterTakeoverReview, confirmation: string, acknowledgeNotImported: boolean, skipPlatformComponents: boolean) {
  if (confirmation !== `take over ${review.cluster.name}`) throw new Error(`Type take over ${review.cluster.name} to confirm ownership.`);
  if (review.cluster.status?.import?.resources.some(resource => resource.disposition === Disposition.NOT_IMPORTED) && !acknowledgeNotImported) throw new Error("Confirm every NOT_IMPORTED resource has another owner or was deleted.");
  return create(TakeOverClusterRequestSchema, { tenantId: review.cluster.tenantId, clusterId: review.cluster.id, etag: review.cluster.etag, acknowledgeNotImported, skipPlatformComponents });
}
