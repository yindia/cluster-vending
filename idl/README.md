# cloud.v1 API contract

`idl/cloud/v1` is the contract of the platform: the Protobuf messages and Connect
services that describe, create, import and delete tenants' cloud accounts,
networks and Kubernetes clusters. Only AWS is implemented. The server translates
the contract into Crossplane managed resources of provider-upjet-aws (namespaced
`*.aws.m.upbound.io` kinds).

The services, the resources, the conventions, the field-to-AWS mapping and the
known gaps (declared but not implemented) are described in
[docs/architecture.md](../docs/architecture.md#the-api), with the
[field mapping](../docs/architecture.md#field-mapping-to-aws).

## Layout

| File | Holds |
|---|---|
| `tenant_service.proto` | `TenantService`; `CloudAccount`, `AwsAccount`, `CloudAccountStatus` |
| `network_service.proto`, `network.proto` | `NetworkService`; `Network`, `NetworkSpec`, `NetworkLayout`, `Subnet`, `NetworkFlowLogs`, `NetworkStatus` |
| `cluster_service.proto`, `cluster.proto` | `ClusterService`; `Cluster`, `ClusterSpec` with its CEL rules, `ClusterStatus`, `ClusterUpgrade` |
| `subtenant_service.proto`, `subtenant.proto` | `SubTenantService`; `SubTenant` and its access types |
| `component_service.proto`, `component.proto`, `component_cloud.proto` | `ComponentService`; `Component`, `ComponentDefinition`, the cloud IAM of a component |
| `admin_service.proto`, `admin_release.proto`, `admin_freeze.proto`, `freeze.proto` | `AdminService`; release and channel views; freeze targets and status |
| `control_plane.proto`, `node_pool.proto`, `node_group.proto`, `access.proto` | Control plane, node pools and node groups, access entries and OIDC |
| `aws.proto` | The AWS provider block: add-ons, control plane log group, launch template, AMI types, AWS status |
| `import.proto`, `plan.proto` | Import plans and dispositions; update plans |
| `managed_resource.proto`, `conditions.proto`, `events.proto` | Managed resource state, conditions, events |
| `deprecation.proto`, `recommendation.proto`, `violation.proto`, `result_index.proto` | Scan results and their index |
| `tenant_directory.proto`, `tenant_dashboard.proto`, `tier.proto`, `code_change.proto`, `cluster_retry.proto` | Tenant read views, the network tier, code changes, cluster retry |
| `options.proto` | `CrossplaneValues`: the string each provider's Crossplane resource expects for an enum value |

## Working on the contract

| Task | Command |
|---|---|
| Generate Go and TypeScript | `make generate` (`buf dep update`, `buf generate`); Go goes to `pkg/gen/go/cloud/v1`, TypeScript to `ui/src/gen`. Both are committed. |
| Lint and check formatting | `make buf-lint` (part of `make check`) |
| Format in place | `make buf-format` |
| Check for breaking changes | `make breaking` (`buf breaking` against `main`; set `AGAINST` for another baseline). Not part of `make check`. |

`buf.gen.yaml` uses remote plugins pinned to the runtime versions in `go.mod`
(`protocolbuffers/go` with `google.golang.org/protobuf`, `connectrpc/go` with
`connectrpc.com/connect/v2`). Bump both together.

Adding a field:

1. Add it with the next free number. Use `optional` for a boolean whose unset
   state must differ from false.
2. Write the comment: what it means, its default, whether it can change after
   creation, and the Crossplane path it maps to.
3. Add validation: `buf.validate` on the field, a CEL rule on the message for
   cross-field constraints, `defined_only` for enums.
4. `make generate`, and commit the generated code with the proto.
5. Add a contract test under `test/idl`: valid values pass, invalid ones are rejected.
6. Handle it in the server: resolve its default (`pkg/network`, `pkg/cluster`), map
   it to the render plan, and, if it can change, add its path to the update handler.
   A field that is unset must leave the rendered resources unchanged.
7. Update [docs/architecture.md](../docs/architecture.md) (the CR definitions and the
   field mapping) for the change.
8. `make check`, and `make breaking` before merging.
