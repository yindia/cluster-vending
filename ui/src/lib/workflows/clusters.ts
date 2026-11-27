import { ClusterState, type Cluster } from "../../gen/cloud/v1/cluster_pb";

/** A cluster the controller is still moving: poll its detail until it settles. */
export function clusterIsConverging(cluster: Cluster): boolean {
  return [ClusterState.PROVISIONING, ClusterState.UPDATING, ClusterState.DELETING].includes(cluster.status?.state ?? 0) ||
    cluster.status?.import?.phase === 1 || cluster.status?.import?.phase === 4 ||
    (cluster.status?.state !== ClusterState.FAILED && cluster.generation > (cluster.status?.observedGeneration ?? 0n));
}
