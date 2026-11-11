# cloud.v1 API contract

The Cluster API is one provider-neutral contract. The server translates it
into Crossplane managed resources for the chosen cloud, and the platform is
their source of truth.

| Provider | Crossplane package | Version checked |
|---|---|---|
| AWS | `crossplane-contrib/provider-upjet-aws` | v2.9.0 |
| GCP | `crossplane-contrib/provider-upjet-gcp` | v3.1.0 |
| Azure | `crossplane-contrib/provider-upjet-azure` | v2.8.0 |

## Resources and services

| Resource | Service | Notes |
|---|---|---|
| `CloudAccount` | `TenantService` | A tenant's cloud account, one per provider. `BootstrapTenant` registers it and prepares the management cluster to reach it; required before any network or cluster. |
| `Network` | `NetworkService` | VPC / VNet with subnets, NAT, routing. Created, or imported and taken over. Shared by clusters. |
| `Cluster` | `ClusterService` | Kubernetes control plane + its default node pool (platform workloads; user workloads run on Karpenter nodes). References a Network by id. |

All RPCs are scoped by `tenant_id` (one rule everywhere: DNS label, ≤ 56
chars, so the namespace `tenant-<tenant_id>` fits 63) and are asynchronous:
poll `Get*` until `status.observed_generation` catches up.

## Prerequisites

### Account access

A tenant has any number of cloud accounts. For each, the tenant gives the
platform admin access (outside this API: e.g. the tenant creates the IAM
role), then the account is onboarded with `TenantService.BootstrapTenant`
before a network is created or imported in it. Bootstrap takes the tenant
ID, the account's provider ID (AWS: account ID) and its name, stores the
account (bootstrap ID, namespace, ProviderConfig name, identity) and creates
in the management cluster:
- the tenant namespace `tenant-<tenant_id>`, if it does not exist;
- one provider credential per account in the tenant namespace, named with a
  generated, stored name (`status.provider_config_name`, e.g.
  `aws-0a1b2c3d`), assuming `status.identity` (AWS: the tenant role ARN):

| Provider | Credential | Access granted by the tenant |
|---|---|---|
| AWS | `ProviderConfig` (aws.m.upbound.io): IRSA + `assumeRoleChain` | IAM role with admin rights, trusted by the platform |
| GCP | `ProviderConfig` (gcp.m.upbound.io): `projectID` + `ImpersonateServiceAccount` | provisioner service account with admin roles |
| Azure | `ProviderConfig` (azure.m.upbound.io): `subscriptionID`, `tenantID`, `clientID`, `OIDCTokenFile` | app registration with Owner (or Contributor + User Access Administrator) |

No static credentials are stored. Each account's bootstrap names the role
to assume in it (AWS: `aws.role`, with `aws.accountId`); its ARN is
stored as the account's identity.

Bootstrap does not install the Crossplane providers or their runtime config:
they are the platform's, installed once per management cluster (AWS:
`provider-aws-ec2`, `-eks`, `-iam` with a `DeploymentRuntimeConfig` whose
service account carries the management role, for IRSA; builders in
`pkg/crossplane/aws`). Without them bootstrap fails with
`FAILED_PRECONDITION`.

How AWS calls are made: the runtime config gives the provider pods the
platform's management role (IRSA); for each managed resource the provider
assumes the tenant role of the resource's ProviderConfig and creates the
resource in that account. One provider install and one runtime config serve
every tenant and account; ProviderConfigs separate them.

Every account has a server-assigned bootstrap ID (`CloudAccount.id`, the
database row ID). Its AWS account ID is unique within the tenant, its name
is free text. A network refers to its account by bootstrap ID
(`NetworkSpec.bootstrap_id`, immutable; for imports
`ImportNetworkRequest.bootstrap_id`); any number of networks can share
an account. A cluster runs in its network's account, a node pool in its
cluster's.

### Before retiring Terraform

The contract owns cloud resources only. Retiring Terraform for a cluster
also needs, outside this API:
- In-cluster software Terraform installed (Helm releases such as the load
  balancer controller, cluster-autoscaler, Karpenter, external-dns,
  metrics-server; Kubernetes manifests) moved to GitOps or Crossplane
  `provider-helm`. These are `NOT_IMPORTED`.
- An owner for every `NOT_IMPORTED` resource in the import plans (e.g.
  application IRSA roles, RDS security groups, Transit Gateway attachments).

## Rules

1. **Common first.** A concept that exists in all three clouds is a field of
   the common spec (`ClusterSpec`, `NetworkSpec`, `ControlPlaneSpec`,
   `NodePoolSpec`), with a neutral name. Each field's comment lists its
   Crossplane path for every provider.
2. **Provider blocks hold only what has no equivalent.** The `provider` oneof
   (`aws`, later `gcp` / `azure`) selects the cloud. Its message carries only
   provider-only knobs, and its field names mirror the Crossplane
   `spec.forProvider` names and nesting (e.g. `access_config.authentication_mode`,
   `upgrade_policy.support_type`).
3. **Enums carry their Crossplane spelling.** Every enum value that ends up
   in a Crossplane string field declares it with
   `[(cloud.v1.crossplane) = {aws: "...", gcp: "...", azure: "..."}]`.
   The server reads the option and never derives a value from the proto name.
   `test/idl/crossplane_contract_test.go` enforces this.
4. **Provider-specific constraints on common fields** are CEL rules on
   `ClusterSpec` and `NetworkSpec`, with IDs prefixed by provider
   (`aws.region`, `aws.subnets_zonal`, ...). Rules that apply to every
   non-AWS provider use `has(this.aws) || ...`.
5. **Units and shapes follow the majority.** `disk_size_gb` (GCP/Azure
   spelling; AWS `diskSize` is also GiB), `scaling_config` with min/max/desired,
   `instance_types` as a list (only AWS accepts more than one).
6. **Zones are zone IDs** (`use1-az1`), never zone names (`us-east-1a`): a
   zone name maps to a different physical zone in each AWS account, a zone ID
   is the same everywhere. The known IDs are in `pkg/constant/aws`.

## Field mapping

Node pool paths are relative to the pool resource. The contract's pool is
always a separate resource: AWS `NodeGroup`, GCP `NodePool` (cluster has
`removeDefaultNodePool: true`), Azure `KubernetesClusterNodePool` in mode User
(AKS's built-in `defaultNodePool` becomes a platform-managed system pool).
`NodePoolSpec` maps this way for `ClusterSpec.default_node_pool`.

| Common field | AWS | GCP | Azure |
|---|---|---|---|
| `spec.region` | `region` | `location` | `location` |
| `control_plane.kubernetes_version` | `Cluster.version`, `NodeGroup.version` | `Cluster.minMasterVersion` (a floor), `NodePool.version` | `kubernetesVersion`, `orchestratorVersion` |
| `control_plane.secrets_encryption.enabled` | `kms` `Key` + `Alias`; `Cluster.encryptionConfig` | Cloud KMS key; `Cluster.databaseEncryption` | Key Vault key; `keyManagementService` |
| `endpoint_access.public_access_cidrs` | `vpcConfig.publicAccessCidrs` | `masterAuthorizedNetworksConfig.cidrBlocks[].cidrBlock` | `apiServerAccessProfile.authorizedIpRanges` (+ the cluster's egress IPs) |
| private nodes, public endpoint (fixed) | `endpointPrivateAccess`, `endpointPublicAccess` = true | `enablePrivateNodes` = true, `enablePrivateEndpoint` = false, `gcpPublicCidrsAccessEnabled` = false | `privateClusterEnabled` = false |
| `NetworkSpec.layout.cidr_block` | `VPC.cidrBlock` | carved into `Subnetwork.ipCidrRange` | `VirtualNetwork.addressSpace` |
| layout `subnets[].cidr_block` | `Subnet.cidrBlock` | `Subnetwork.ipCidrRange` | `Subnet.addressPrefixes` |
| layout `subnets[].role` | PUBLIC / PRIVATE | PRIVATE only | PRIVATE only |
| layout `subnets[].zone_id` | `Subnet.availabilityZoneId` (required) | empty (regional) | empty (regional) |
| layout `availability_zone_count` | subnet pairs per zone | `Cluster.nodeLocations` | pool `zones` |
| layout `nat_gateway_mode` | `NATGateway.availabilityMode` + `EIP` | `Router` + `RouterNAT` + `Address` (REGIONAL only) | `NATGateway` + `PublicIP` + associations (SINGLE) |
| `network.network_id` | the Network's private `Subnet`s → `vpcConfig.subnetIds`, `NodeGroup.subnetIds` | its `Subnetwork` → `Cluster.subnetwork` | its `Subnet` → `vnetSubnetId` |
| `network.zone_ids` | private subnets of those zones only (≥ 2) | `Cluster.nodeLocations` | pool `zones` |
| `network.pod_cidr` | must be empty (VPC CNI) | `Subnetwork.secondaryIpRange` "pods" | `networkProfile.podCidr` (overlay) |
| `network.service_cidr` | `kubernetesNetworkConfig.serviceIpv4Cidr` | `Subnetwork.secondaryIpRange` "services" | `networkProfile.serviceCidr`, `dnsServiceIp` |
| pool `instance_types` | `instanceTypes` | `nodeConfig.machineType` | `vmSize` |
| pool `capacity_type` | `capacityType` | `nodeConfig.spot` | `priority` |
| pool `scaling_config.{min,max}_size` | `scalingConfig.{min,max}Size` | `autoscaling.total{Min,Max}NodeCount` | `{min,max}Count`, `autoScalingEnabled` |
| pool `scaling_config.desired_size` (create-only) | `initProvider.scalingConfig.desiredSize` | `initProvider.initialNodeCount` (per zone) | `initProvider.nodeCount` |
| pool `disk_size_gb` | `diskSize` | `nodeConfig.diskSizeGb` | `osDiskSizeGb` |
| pool `labels` | `labels` | `nodeConfig.labels` | `nodeLabels` |
| pool `taints` | `taint[]` | `nodeConfig.taint[]` | `nodeTaints` |
| `labels` (as cloud tags) | `tags` | `resourceLabels` | `tags` |
| `spec.access.admin_principals` | `AccessEntry` + `AccessPolicyAssociation` (AmazonEKSClusterAdminPolicy) | cluster-admin `ClusterRoleBinding` | `azureActiveDirectoryRoleBasedAccessControl.adminGroupObjectIds` |
| `status.network.egress_ips` | `EIP.atProvider.publicIp` | `Address.atProvider.address` | `PublicIP.atProvider.ipAddress` |

Per-provider settings that stay out of the common spec and land in the
provider block (`gcp` / `azure`) when those providers ship: GCP release
channel, image type; Azure `sku_tier`, system pool size, upgrade channels.

## Networks

The platform is the source of truth for every network it serves; there is
no read-only "bring your own network".

| How | Call | Owner |
|---|---|---|
| Created by the platform | `CreateNetwork` | the platform; `DeleteNetwork` refused while clusters use it |
| Built elsewhere (e.g. Terraform) | `ImportNetwork` → review `status.import` → `terraform state rm` → `TakeOverNetwork` | the platform, after the takeover |

Clusters only reference a network: `ClusterSpec.network.network_id`, the
zones to use (`zone_ids`, default all) and the cluster's own `pod_cidr` /
`service_cidr`. The layout is never repeated in a cluster request, and one
network can serve several clusters.

- `Network.id` is assigned by the server (time-ordered UUID) and every
  call uses it; the client names the network (`Network.name`, not unique,
  the prefix of its resource names). The request as sent is stored too.
- Network resources carry the label `cluster-vending.io/network: <id>`;
  clusters select the network's private subnets (in `zone_ids`) by it.
- `ImportNetwork` takes the VPC ID (and optionally which subnets). The
  server looks the network up once, read-only, with the tenant's
  credentials, because Crossplane has no lookup kinds, and creates one
  Observe-only managed resource per result (`managementPolicies: [Observe]`,
  `crossplane.io/external-name` = cloud ID). Nothing changes in the cloud.
  From then on status comes from `status.atProvider`; the lookup never runs
  again. The spec is derived from `atProvider` (VPC CIDR, subnets with role
  from routing, NAT mode). What is modelled and what is carried over is
  listed in `AwsImportNetwork` (`aws.proto`).
- The VPC's main route table is imported as `DefaultRouteTable`: a
  `RouteTable` resource could never be deleted, so `DeleteNetwork` would hang.
- `TakeOverNetwork` fills `forProvider` from `status.atProvider`, switches
  to `["*"]` and converges the plan, e.g. adding the
  `kubernetes.io/role/elb` / `internal-elb` subnet tags. Imported resources
  keep their cloud names and tags. From then on Crossplane reverts any
  change made outside the platform.
- Egress IPs, NAT gateways and subnet IDs come from the platform's own
  resources: `Network.status.resources`, and per cluster
  `ClusterStatus.network`.

## Node pools

- A cluster has one node pool: `ClusterSpec.default_node_pool`, created with
  the cluster and changed through `UpdateCluster`. It runs platform
  workloads; user workloads run on Karpenter nodes, so there is no
  node pool API.
- Changing instance types, capacity type or disk size replaces the node
  group: a new one is created and the old one deleted once the new one is
  ready.
- Other node groups of an imported cluster are carried over verbatim
  (`status.carried_over`) and deleted with the cluster.
- `DeleteCluster` deletes every node group.

## How changes roll out

- **Reads change nothing.** `Get*`, `List*` and `Watch*` only read (managed
  resources from the server's informer cache). A background loop in the
  server (every 30 s, one replica at a time through leader election) moves
  on what is not settled: it applies a generation again until every object
  of it was applied, creates add-ons and access entries once EKS is ACTIVE,
  retires a replaced node group, continues deletions and removes the row
  when everything is gone.
- **Status.** `status.observed_generation` is the last generation that
  converged: all of it applied, and every managed resource's provider has
  reconciled its latest spec (`ManagedResource.generation` =
  `observed_generation`). `RUNNING` needs that and every resource Ready.
- **Kubernetes upgrades** are staged: the control plane first; node groups
  and add-ons render at the version the control plane reports until it
  reports the target. The version-skew errors EKS returns meanwhile are
  `UPDATING`, not `FAILED`.
- **Node count.** `scaling_config.desired_size` is sent at creation only
  (`initProvider`); afterwards the node count is the autoscaler's.
- **Plan before update.** `UpdateCluster` with `validate_only` returns a
  `Plan` and changes nothing: every resource that would be created,
  changed in place, changed disruptively, deleted, or that the cloud cannot
  change (`REJECTED_IMMUTABLE`; the plan is then not `applicable`), with a
  diff, including fields changed outside the platform that would be set
  back. Passing `plan_id` back applies only if nothing changed since.
- **Deletion order** of a cluster is enforced by Crossplane `Usage`
  objects rendered with it (the EKS cluster is used by its node groups,
  the IAM roles by the cluster and node groups, the network's subnets by
  the cluster, ...): all objects are deleted at once, each cloud resource
  goes only when its users are gone. `DeleteClusterRequest.force` also
  releases Helm releases without uninstalling, for a cluster whose API is
  unreachable. A deleting cluster's `status.message` names what it waits
  for and what is stuck, with the error and for how long.
- **Renderer changes** roll out resource by resource: every row stores the
  renderer version it was applied with; the loop re-applies rows below the
  server's target (`--render-target`, limited by `--render-rollout`).
- **Provider upgrades**: `AdminService.PauseReconcile` /
  `ResumeReconcile` set or remove `crossplane.io/paused` on a tenant's or
  one cluster's managed resources.

## Imports

`ImportStatus` (`cloud/v1/import.proto`) is the plan of a network import
(permanent) and of a cluster import (temporary). The rules, for both:

- **Keep current.** An optional spec field left empty keeps the current
  value (Crossplane late-initialization), never the server default. An
  imported cluster without `kubernetes_version` stays on its version; the
  same for authentication mode, support type, log types, encryption,
  endpoint access and add-ons. Only fields that are set can change.
- **Tags are kept.** Every existing tag survives the takeover (`Name`,
  `karpenter.sh/discovery`, `kubernetes.io/cluster/<name>`, ...); the
  platform only adds its own ownership tags and the labels on top. Removing
  a label later removes only that tag.
- **Closure.** Everything an imported resource references is imported too.
  What the contract models becomes part of the spec; the rest is
  `CARRIED_OVER`. Resources that only reference ours stay with their owner
  (`NOT_IMPORTED`). After a takeover, the previous tool has nothing left to
  manage for the imported stack.

| Disposition | Meaning |
|---|---|
| `MATCHES` | Imported; already what the spec says. |
| `WILL_UPDATE` | Imported; the takeover changes it (`detail` has the diff). `requires_replacement` marks changes that delete and recreate the resource (e.g. node group instance types): review those first. |
| `WILL_CREATE` | Missing; created at the takeover (tags, add-ons, access entries, Usages, a new node pool). |
| `NOT_IMPORTED` | Found next to the imported resources but outside the contract and not referenced by them. Needs another owner or deletion before the previous tool goes. |
| `BLOCKING` | Can't be expressed in the contract; phase `BLOCKED`. |
| `CARRIED_OVER` | Imported and taken over verbatim although no spec field describes it: owned and deleted with the Network / cluster, listed in `status.carried_over`, not editable through the API. |

Carried over (AWS):

| Import | Resources |
|---|---|
| Network | routes other than `0.0.0.0/0` in imported route tables (peering, Transit Gateway, VPN; the targets are `NOT_IMPORTED`); VPC endpoints on imported subnets / route tables; flow logs with their log group and IAM role; the default security group and default network ACL (`DefaultSecurityGroup`, `DefaultNetworkACL`, rules inline) |
| Cluster | security groups referenced by the cluster or node groups, transitively through their rules, with every rule (`SecurityGroupIngressRule` / `SecurityGroupEgressRule` by `sgr-` ID; avoid the legacy `SecurityGroupRule`, its ID is composite), plus rules added to the EKS-managed cluster group (the group stays with EKS); the KMS key in `encryptionConfig` and its aliases (EKS can never turn encryption off); the log group `/aws/eks/<name>/cluster`; the IAM OIDC provider of the cluster's issuer; IAM roles of add-ons and pod identity associations; node groups with launch templates or custom AMIs, with their launch templates |

`BLOCKING` today (the layout can't express them): secondary VPC CIDRs (e.g.
`100.64.0.0/16` for EKS custom networking), IPv6 / dual-stack, private
egress through a Transit Gateway or firewall instead of a NAT gateway, a VPC
CIDR outside /16-/24, private subnets in a single zone.

AWS import IDs (`crossplane.io/external-name`) and the `forProvider` fields
the provider builds them from (needed even for Observe-only):

| Kind | external-name | forProvider |
|---|---|---|
| VPC, Subnet, InternetGateway, EIP, NATGateway, RouteTable, DefaultRouteTable, DefaultNetworkACL, DefaultSecurityGroup, SecurityGroup, VPCEndpoint, FlowLog, LaunchTemplate | cloud ID (`vpc-…`, `subnet-…`, `eipalloc-…`, `rtb-…`, `acl-…`, `sg-…`, `vpce-…`, `fl-…`, `lt-…`) | `region` (+ `vpcId` for the default kinds) |
| SecurityGroupIngressRule, SecurityGroupEgressRule | `sgr-…` | `region`, `securityGroupId` |
| Route | `<rtb-id>_<destination cidr>` | `routeTableId`, `destinationCidrBlock` |
| RouteTableAssociation | association ID `rtbassoc-…` (not Terraform's `subnet/rtb` import shorthand) | `subnetId`, `routeTableId` |
| Role | role name | — |
| OpenIDConnectProvider | provider ARN | — |
| Key (KMS) | key ID | `region` |
| Alias (KMS) | `alias/<name>` | `region` |
| Group (CloudWatch Logs) | log group name | `region` |
| Cluster | cluster name | `region` |
| NodeGroup | node group name | `region`, `clusterName` |
| Addon | `<cluster>:<addon>` | `clusterName`, `addonName` |
| AccessEntry | `<cluster>:<principal arn>` | `clusterName`, `principalArn` |
| AccessPolicyAssociation | `<cluster>#<principal arn>#<policy arn>` | `clusterName`, `principalArn`, `policyArn` |
| PodIdentityAssociation | association ID | `clusterName` |

### Cluster import (temporary)

Clusters built with Terraform are imported with one temporary field,
`ClusterSpec.existing_cluster` (`cloud/v1/cluster_import.proto`), through
the normal `ClusterService`.

| Step | Call | Crossplane on the imported resources |
|---|---|---|
| 0. Network | `ImportNetwork` + `TakeOverNetwork` for the cluster's VPC | see [Networks](#networks) |
| 1. Import + plan | `CreateCluster` with `spec.existing_cluster` and `network.network_id` | one read-only lookup, then Observe-only resources for everything modelled and carried over. Nothing changes. |
| 2. Review | `GetCluster` (`status.import`) | check `requires_replacement` and `NOT_IMPORTED` |
| 3. Hand off | `terraform state rm` | — |
| 4. Take over | `UpdateCluster` `spec.existingCluster.policy` = `MANAGE_NO_DELETE` | `[Observe, Create, Update, LateInitialize]`; `forProvider` filled from `atProvider` first, then the plan converges |
| 5. Own | `UpdateCluster` policy `FULL` + `acknowledgeNotImported` | `["*"]` |
| 6. Finish | `UpdateCluster` clearing `spec.existingCluster` | needs phase `CONVERGED`; Terraform can be removed |

- The node group named in `existing_cluster.default_node_pool_name` must be
  representable (AWS: no launch template, no custom AMI); otherwise leave it
  empty and `default_node_pool` is created as a new pool.
- Access: the platform's account access lets it grant itself cluster-admin
  (an `AccessEntry` + `AccessPolicyAssociation` for the provisioner role,
  `WILL_CREATE`); nothing is needed on the Terraform side.
- A cluster in `CONFIG_MAP` mode: the platform reads `kube-system/aws-auth`
  through the Kubernetes API and plans an access entry per mapped role and
  user (`WILL_CREATE`), plus the switch to `API_AND_CONFIG_MAP`
  (`WILL_UPDATE`; EKS only allows that direction).

**Dropping cluster import** once no cluster has `spec.existing_cluster` set:
1. Delete `cloud/v1/cluster_import.proto` and the fields
   `ClusterSpec.existing_cluster` (7) and `ClusterStatus.import` (13); add
   `reserved 7; reserved "existing_cluster";` to `ClusterSpec` and
   `reserved 13; reserved "import";` to `ClusterStatus`. Remove the import of
   `cluster_import.proto` from `cluster.proto` (keep `import.proto`:
   `ClusterStatus.carried_over` and network imports use it).
2. Delete `test/idl/cluster_import_test.go` and this section.
3. `buf lint` and `buf breaking` pass: `buf.yaml` uses `WIRE_JSON`, which
   allows deleting a field whose number and name are reserved.

## Naming

Kubernetes and cloud names are derived by the server, never taken from the
caller verbatim, so they always fit the provider limits. `hash8` is the first
8 hex chars of `sha256(tenant_id/cluster_id)`: it keeps names unique across
tenants sharing a cloud account; the tenant stays visible through the
`cluster-vending.io/tenant` tag or label.

| Object | Name | Limit |
|---|---|---|
| Namespace | `tenant-<tenant_id>` (tenant_id ≤ 56) | 63 |
| Managed resource `metadata.name` (networks) | `<network name>-<hash8 of tenant_id/network id>-<suffix>`; `Network.id` is server-assigned, the name is the client's (not unique) | 253 |
| Managed resource `metadata.name` (clusters) | `<cluster_id>-<suffix>` | 253 |
| AWS: EKS cluster, IAM roles | `<cluster_id>-<hash8>[-role]` | EKS 100, IAM 64 |
| AWS: node groups | `default`, or the NodePool id | 63 |
| GCP: GKE cluster | `<cluster_id truncated to 31>-<hash8>` | 40, lowercase, starts with a letter |
| GCP: service account ID | `cv-<hash8>-nodes` | 6-30 |
| Azure: AKS cluster, `dnsPrefix` | `<cluster_id>-<hash8>` | 63, 54 |
| Azure: resource groups | `rg-<cluster_id>-<hash8>[-nodes]` | 90 |
| Azure: node pools | `system`, `user`, or the NodePool id truncated | 12, lowercase alphanumeric |
| Imported resources | unchanged cloud names | — |

## Server-side rules the schema can't express

- Subnet CIDRs inside the layout's `cidr_block` and non-overlapping;
  `pod_cidr` / `service_cidr` not overlapping the network.
- A cluster on `network.network_id`: the Network exists in the tenant, is
  `READY` (an `IMPORTED` one only for a cluster import), and has the
  cluster's region and provider; every `zone_ids` entry is a zone of the
  network with a private subnet.
- Default zones for `availability_zone_count` are the first zone IDs of the
  region in `pkg/constant/aws`. A zone the account cannot use (constrained
  zones, e.g. one of us-west-1's three for newer accounts) shows up as a
  subnet creation error; the server then moves on to the next zone ID.
- `endpoint_access.public_access_cidrs` has no default: at least one CIDR
  is required (`["0.0.0.0/0"]` for any address), so who can reach a
  cluster's API is always an explicit choice.
- `kubernetes_version` is resolved to a concrete version and written to the
  control plane and every node pool.
- `UpdateCluster` ignores `scaling_config.desired_size`: it
  is create-only (`initProvider`) and the cluster autoscaler owns the node
  count afterwards. GCP `initialNodeCount` is per zone:
  `ceil(desired_size / zones)`.
- Add-on versions (AWS `Addon.addonVersion`) are set explicitly to the
  default for the cluster's Kubernetes version and bumped on every upgrade.
  An omitted version would be late-initialized and pinned.
- `secrets_encryption.enabled` can go from false to true, never back.
- GKE `deletionProtection` defaults to true: the server sets it to false, or
  `DeleteCluster` could never finish.
- Azure needs a second pass for values that exist only after creation:
  `RoleAssignment.principalId` (from the identity's `atProvider.principalId`)
  and the NAT egress IP appended to `authorizedIpRanges` (from
  `PublicIP.atProvider.ipAddress`).
- GCP label charset for `labels` (`[a-z0-9_-]`, key starts with a letter):
  a `gcp.*` CEL rule when GCP ships.

### Read-only lookups the server makes

Crossplane has no lookup kinds, so the server reads these itself, once, with
the tenant's credentials; everything else goes through Crossplane. They are
part of the admin access granted at onboarding.

| When | AWS calls |
|---|---|
| `ImportNetwork` | `ec2:DescribeSubnets`, `DescribeRouteTables`, `DescribeInternetGateways`, `DescribeNatGateways`, `DescribeAddresses`, `DescribeSecurityGroups`, `DescribeNetworkAcls`, `DescribeVpcEndpoints`, `DescribeFlowLogs`; `logs:DescribeLogGroups`; `iam:GetRole` |
| Cluster import (temporary) | `eks:DescribeCluster`, `ListNodegroups`, `DescribeNodegroup`, `ListAddons`, `DescribeAddon`, `ListAccessEntries`, `ListAssociatedAccessPolicies`, `ListPodIdentityAssociations`; `iam:ListAttachedRolePolicies`, `ListOpenIDConnectProviders`; `kms:DescribeKey`, `ListAliases`; `logs:DescribeLogGroups`; `ec2:DescribeSecurityGroups`, `DescribeSecurityGroupRules`, `DescribeLaunchTemplateVersions`; Kubernetes `kube-system/aws-auth` for `CONFIG_MAP` clusters |

## Always created (not in the contract)

| | AWS | GCP | Azure |
|---|---|---|---|
| Control-plane identity | IAM role + `AmazonEKSClusterPolicy` | (Google-managed) | `UserAssignedIdentity` + `RoleAssignment` Network Contributor |
| Node identity | IAM role + worker / CNI / ECR policies | `ServiceAccount` + `ProjectIAMMember` (defaultNodeServiceAccount, artifactregistry.reader) | (managed) |
| Add-ons | `Addon` x5: vpc-cni, coredns, kube-proxy, eks-pod-identity-agent, aws-ebs-csi-driver | built in | built in |
| Block storage driver | `aws-ebs-csi-driver` with its IAM role (`AmazonEBSCSIDriverPolicy`) and a `PodIdentityAssociation` for `kube-system/ebs-csi-controller-sa`: EBS persistent volumes need it since Kubernetes 1.23 | built in | built in |
| Secrets encryption key (when enabled) | `kms` `Key` + `Alias` | Cloud KMS key | Key Vault key |
| Extra pools / groups | — | — | `ResourceGroup`, system `defaultNodePool` |
| Platform access | `bootstrapClusterCreatorAdminPermissions: true` | provisioner SA (`roles/container.admin`) | provisioner app registration |
| Delete order | `Usage`: node pools → control plane → identity, NAT, subnets, network | same | same |

## Adding a provider

1. Add `cloud/v1/<provider>.proto` with `<Provider>ClusterSpec`,
   `<Provider>ClusterStatus` and `<Provider>NodePoolSpec`, holding only
   settings missing from the common
   spec (e.g. GCP `release_channel`, `image_type`; Azure `sku_tier`).
2. Add the arm to the `ClusterSpec.provider`, `ClusterStatus.provider`,
   `NetworkSpec.provider` and `ImportNetworkRequest.provider` oneofs (11 =
   gcp, 12 = azure). They are not `reserved`, because un-reserving is a
   breaking change for buf.
3. Fill in the provider's `crossplane` value on the common enums
   (`CapacityType`, `TaintEffect`, `NatGatewayMode`) and extend the contract
   test.
4. Add `<provider>.*` CEL rules on `ClusterSpec` and `NetworkSpec`
   for its constraints on the common fields (region format, layout, zones,
   GCP label charset, ...).
5. Add its region / zone catalog next to `pkg/constant/aws`.

## Codegen

`buf generate` writes Go code to `pkg/gen/go` (import path
`github.com/yindia/cluster-vending/pkg/gen/go/...`) with remote plugins on the
Buf Schema Registry, pinned to the runtime versions in `go.mod`
(`protocolbuffers/go` ↔ `google.golang.org/protobuf`, `connectrpc/go` ↔
`connectrpc.com/connect/v2`). Bump both together. Unauthenticated BSR use is
rate-limited; run `buf registry login` in CI.
