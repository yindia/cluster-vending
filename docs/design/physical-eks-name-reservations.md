# Physical EKS name reservations and account creation fences

The Kubernetes store reserves each `(AWS partition, canonical AWS account ID,
region, exact EKS name)` in a ConfigMap in **`kube-system`**. This namespace is a
fixed coordination domain, independent of tenant namespaces, account registration
UUIDs, and controller leader-election settings. Every API/controller replica must
use this same management cluster and reservation namespace. Names and existing
cluster UUIDs are not changed.

ConfigMaps have label `cloud.cluster-vending.io/eks-name-reservation: "true"` and
names `cv-eks-name-<SHA-256 prefix>`. Their data retains the complete scope key,
tenant, allocation UUID, request UUID and normalized request hash, so a truncated
hash collision fails closed. Errors returned to tenant callers disclose no other
tenant or allocation identity. There are deliberately no cross-namespace owner
references and no age-based reclamation.

## Existing and imported resources

Before taking a reservation, the store checks existing Cluster CRs across tenant
namespaces and resolves their network/account references. An existing registration
blocks a competing allocation even when it predates reservations. Native adoption
and reconciliation also take/check the reservation before cloud writes. Imports
reserve the same physical identity as new creates; they cannot establish a second
owner. Existing conflicting registrations stop for operator review, without
renaming or adopting either physical resource.

The normal finalization path releases a reservation only after both cluster and
platform inventory have been cleared and the management Cluster CR is confirmed
gone. A foreign finalizer retains the reservation. Removing a CR directly or
losing the management cluster is not evidence that the AWS cluster is absent.

## Crash recovery

After a crash between reservation and Cluster persistence, retrying the exact
create request UUID and payload recovers the reserved allocation UUID. A different
request can take the name only once the reservation is clearly abandoned: it has
no cluster UID (no Cluster object ever held it), no Cluster with its allocation
UUID exists in its tenant, and it was taken more than 10 minutes ago
(`reservedAt`, else the ConfigMap's creation time). Nothing reached the cloud for
such a reservation, because cloud resources come only from a stored Cluster. It
is deleted with its UID and resourceVersion as preconditions, and the
legacy-owner check still runs before the name is taken. A reservation a Cluster
ever held is never reclaimed this way. Reservations also remain on ambiguous storage or
cleanup failures; they are not leases.

For an orphaned reservation, an operator must inspect its complete scope and owner,
the management Cluster/inventory, and the actual EKS identity before any cleanup.
Confirm AWS absence and all managed-resource cleanup, or restore the original
registration and let normal finalization complete. Only then remove the specific
reservation using its UID/resourceVersion preconditions. Never reclaim by age,
delete all claims, or adopt an EKS cluster merely because its name matches. A
foreign-finalizer completion after the store's finalization attempt may require
this explicit verified cleanup.

Release is two-phase, so the interval between deleting the management Cluster CR
and deleting its reservation is covered. Once the cluster's inventory is
finalized (every cloud resource gone), the reservation is marked `releasing`
(a durable tombstone) before the CR is removed; then the CR goes, then the
reservation. If the process crashes or the reservation delete fails in that
interval, the next create of the name finds a reservation marked `releasing`
whose Cluster is gone and takes it over at once (deleted with UID and
resourceVersion preconditions). A reservation without the mark whose Cluster
disappeared (for example a CR whose finalizer was removed by hand) proves
nothing about the cloud side: it still fails closed and needs the verified
operator procedure above.

## Account deletion versus network creation

Network insertion/adoption first rejects a deleting account, persists pending
intent, then updates the same account incarnation using a resource-version CAS.
The pending Network cannot be read as provisionable store state. Once the CAS
succeeds, the pending marker is cleared. Account deletion prepared before insertion
then has an obsolete resourceVersion; deletion that won first makes creation
refuse and take back its pending intent. Exact create retries and mutating network
reconciliation resume interrupted fences. Get/List remain read-only.

The fence and a network's visibility are separate Kubernetes writes, not a
transaction. Pending-state checks are required before cloud application. If a
concurrent finalizer prevents rollback, pending intent stays blocked for recovery.

## Credentials, RBAC and remaining boundaries

`management.ConnectStore` constructs the store with a direct, uncached Kubernetes
client. Reservation and ownership decisions must never use informer-cache reads.
The server's existing ClusterRole/ClusterRoleBinding permits ConfigMap get/create/
delete in `kube-system`; the controller's platform role additionally permits the
list/update/patch operations used by store recovery and finalization. Tenant
namespace-only RoleBindings are insufficient. Deployment checks exercise these
permissions. Restrict direct editing of reservation ConfigMaps to trusted platform
operators; Kubernetes RBAC cannot grant create permission by label prefix.

These guarantees cover cooperating API/controller writers in one management
cluster. Kubernetes has no transaction spanning a reservation, Cluster, account
and Network object. The fences prevent unsafe application while incomplete, but
partial intent and operator recovery remain possible. Independent management
clusters and actors creating EKS clusters directly in AWS do not participate in
this reservation protocol. The existing AWS existence check remains necessary,
and an external actor can still race that check: no atomic AWS/Kubernetes ownership
transaction is claimed. Proving ownership before adopting an externally created
same-name EKS cluster requires a separate provider-side ownership handshake.
