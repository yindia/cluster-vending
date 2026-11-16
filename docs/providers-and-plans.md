# Provider adapters, inventory and update plans

The active provisioning backend is AWS. Recognizing a GCP provider ID does not
enable GCP provisioning. A GCP backend must supply account authentication and
discovery, API validation, resource rendering, provider installation and schema
registration, readiness/repair logic, and field safety classifications.

The shared planning engine in `pkg/crossplane/planning` works on Kubernetes
objects. It accepts provider-specific safety policies keyed by full group,
version and kind; fields without a safety classification are rejected. Inventory
and plan identity include API version, kind, namespace and name, so AWS and GCP
resources with the same kind and name remain distinct. Freeze accepts the active
provider's managed kinds through `reconcile.Deps.ManagedResourceKinds`; the
default remains AWS plus Helm. A future backend must register every managed kind
it uses, including list types, so freeze can discover and pause those resources.

## Inventory storage

There is no external database. The repository implementation in
`pkg/database/cloudstore/managed.go` persists inventory in Kubernetes status:

| Owner | Inventory field |
| --- | --- |
| CloudAccount | `status.inventory` |
| Network | `status.inventory` |
| Cluster infrastructure | `status.inventory` |
| Cluster platform components | `status.componentInventory` |
| Component | `status.inventory` |

Each entry records the managed resource's API version, kind, namespace, name,
external cloud identity and carried-over state. The managed resource objects
themselves live in the management Kubernetes cluster, usually in the tenant's
namespace. Their spec and status remain Crossplane's source of desired and
observed cloud state. Inventory records ownership and supports inspection,
pruning, deletion and freeze; it is not a separate copy of all cloud state.

For example, inspect an owner with:

```sh
kubectl --context kind-cluster-vending -n tenant-acme get networks <kubernetes-name> -o yaml
kubectl --context kind-cluster-vending -n tenant-acme get clusters <kubernetes-name> -o yaml
```

## Reviewing YAML updates

Both commands preview resource changes without recording an update:

```sh
cluster-vending cluster update --id <cluster-id> -f cluster.yaml --plan
cluster-vending network update --id <network-id> -f network.yaml --plan
```

The CLI infers an update mask from fields present in the manifest. Omitting a
field leaves it unchanged; clearing a collection requires explicitly setting it
empty. The resulting desired resource set is compared with the owner's stored
inventory. Removed items appear as `DELETE`, alongside creates, field updates,
disruptive changes and rejected immutable changes. For example, disabling network
flow logs lists removal of the FlowLog and its dedicated delivery resources.
Deletion details distinguish deleting the cloud resource from retaining it under
an explicit retention policy. Carried-over inventory is excluded from pruning.

A normal update also prints the resource plan before submitting the update. To
apply only a plan previously reviewed, include `--plan-id <id>` with the same
manifest. The server recomputes the plan and rejects it if the request, rendered
desired state, owner version or observed managed-resource versions changed.
This includes versions of deletion targets. Preview uses reads and dry-run apply;
it does not persist desired state or managed resources. Resource versions can
change during ordinary Crossplane reconciliation, requiring a fresh preview.

The RPC contract uses `validate_only` for previews and `plan_id` to bind updates
to the reviewed snapshot. For compatibility, direct RPC callers can still submit
an update without a plan ID. `update_recorded` means the desired configuration
was accepted; cloud convergence is asynchronous. `--wait` waits for readiness.

Plans describe the observed snapshot, not a transaction across Kubernetes and
the cloud. Freeze and rollout gates still control when reconciliation runs.
Tier or release-policy changes have no immediate managed-resource diff; the plan
explains that subsequent automatic changes follow their normal rollout gates.
