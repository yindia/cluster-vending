import type { ManifestKind } from "./manifest-yaml";

/**
 * Starter manifests: the examples/*.yaml the CLI takes, with the active tenant filled in
 * and the IDs as placeholders to replace. Optional settings stay commented out.
 */
const templates: Record<ManifestKind, (tenant: string) => string> = {
  CloudAccount: (tenant) => `# Connect an existing AWS account to the tenant (cluster-vending account create -f).
# Replace the account ID (AWS's documentation placeholder) and the role with your own.
# The answer has the account's platform ID: the bootstrapId network manifests refer to.
apiVersion: cluster-vending.io/v1
kind: CloudAccount
metadata:
  name: production            # account name, up to 128 characters (not unique)
  tenant: ${tenant}
spec:
  aws:
    accountId: "111122223333"   # REPLACE: the 12-digit AWS account ID
    role: cluster-vending-provisioner   # role name (no ARN or path) that trusts the platform
# After connecting, the account's Access tab gives the CloudFormation template for the role.
`,
  Network: (tenant) => `# A network in one of the tenant's accounts (cluster-vending network create -f).
apiVersion: cluster-vending.io/v1
kind: Network
metadata:
  name: prod                  # immutable; lowercase DNS label, 1-40 characters
  tenant: ${tenant}
  displayName: Prod network   # optional, at most 128 characters
  labels: {env: prod}         # filtering and AWS tags on every resource; at most 40 pairs
spec:
  # REPLACE: the platform account ID (a UUID, not the AWS account number); see References.
  bootstrapId: 00000000-0000-0000-0000-000000000000
  region: us-west-1           # immutable; must match the account's verified region
  # Inherited by every cluster in this network: TIER_2 (first), TIER_1 (next), TIER_0 (last, default).
  tier: TIER_2
  # Immutable after creation. Omit layout for 10.0.0.0/16, 3 zones, one NAT.
  layout:
    cidrBlock: 10.0.0.0/16    # IPv4 /16 through /24; automatic subnets need /22 or larger
    availabilityZoneCount: 3  # 2-6; omit when you list subnets
    natGatewayMode: NAT_GATEWAY_MODE_SINGLE   # or NAT_GATEWAY_MODE_PER_ZONE
    # Explicit subnets instead of a zone count (zone IDs, not names like us-west-1a):
    # subnets:
    #   - {role: SUBNET_ROLE_PUBLIC,  cidrBlock: 10.0.0.0/24,  zoneId: usw1-az1}
    #   - {role: SUBNET_ROLE_PRIVATE, cidrBlock: 10.0.16.0/20, zoneId: usw1-az1}
    #   - {role: SUBNET_ROLE_PRIVATE, cidrBlock: 10.0.32.0/20, zoneId: usw1-az3}
  # Optional VPC flow logs to a dedicated CloudWatch Logs group:
  # flowLogs:
  #   enabled: true
  #   trafficType: FLOW_LOG_TRAFFIC_TYPE_ALL      # or _ACCEPT, _REJECT
  #   retentionDays: 30                           # 1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, ...
  #   aggregationIntervalSeconds: 600             # 60 or 600
# Release channels belong to each cluster. status, etag and timestamps are server-owned.
`,
  Cluster: (tenant) => `# An EKS cluster in one of the tenant's networks (cluster-vending cluster create -f).
apiVersion: cluster-vending.io/v1
kind: Cluster
metadata:
  name: prod-eks              # the EKS cluster name; lowercase DNS label, 1-40 characters
  tenant: ${tenant}
  displayName: Prod EKS
  labels: {env: prod}         # filtering, and AWS tags on every resource
spec:
  region: us-west-1           # the network's region
  network:
    networkId: 00000000-0000-0000-0000-000000000000   # REPLACE: the network's ID; see References
    # Immutable: private subnets in these zones (zone IDs, at least two); empty uses all.
    # zoneIds: [usw1-az1, usw1-az3]
    # serviceCidr: 172.20.0.0/16   # immutable, /12 through /24; omit for the EKS default
  controlPlane:
    # kubernetesVersion: "1.35"    # a minor; omit for the channel's. Ignored with UPGRADE_MODE_AUTOMATIC.
    endpointAccess:
      endpointPublicAccess: true
      endpointPrivateAccess: true
      publicAccessCidrs: ["203.0.113.0/24"]   # REPLACE: 1-40 IPv4 CIDRs allowed to reach the public API
    secretsEncryption: {enabled: true}   # a KMS key the platform creates; cannot be turned off later
  releaseChannel: RELEASE_CHANNEL_STABLE  # or RELEASE_CHANNEL_BETA, RELEASE_CHANNEL_ALPHA
  upgradeMode: UPGRADE_MODE_MANUAL        # UPGRADE_MODE_AUTOMATIC follows the channel's minor
  # Extended support: a minor the release offers on no channel runs with UPGRADE_MODE_MANUAL
  # and spec.aws.upgradePolicy.supportType AWS_SUPPORT_TYPE_EXTENDED only.
  # maintenanceWindow:                      # when an automatic upgrade may start (UTC)
  #   days: [WEEKDAY_SATURDAY, WEEKDAY_SUNDAY]
  #   start: "02:00"
  #   duration: 4h
  access:
    # Cluster admins: IAM role or user ARNs. Only the platform's own role is an admin by
    # default: add your own IAM principal here, or no one else can use kubectl.
    # Up to 20; review warns while the list is empty.
    adminPrincipals: []
    # adminPrincipals:
    #   - arn:aws:iam::111122223333:role/platform-admin
    # accessEntries:                       # other principals with scoped EKS access policies
    #   - principalArn: arn:aws:iam::111122223333:role/developers
    #     policies:
    #       - policyArn: arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy
    #         scope: ACCESS_POLICY_SCOPE_NAMESPACE
    #         namespaces: [team-a]
  aws:
    irsa: true                # the IAM OIDC provider platform components' roles need
    controlPlaneLogGroup: {enabled: true, retentionDays: 30}
    enabledClusterLogTypes: [AWS_CLUSTER_LOG_TYPE_API, AWS_CLUSTER_LOG_TYPE_AUDIT, AWS_CLUSTER_LOG_TYPE_AUTHENTICATOR]
    upgradePolicy:
      supportType: AWS_SUPPORT_TYPE_STANDARD   # AWS_SUPPORT_TYPE_EXTENDED on an extended-support minor
    # EKS add-ons, merged by name into the default set (vpc-cni, coredns, kube-proxy, ...):
    # addons:
    #   - name: metrics-server
# Node groups are the platform's, not part of the spec: every cluster gets the system node
# group and Karpenter's platform pool. Do not set nodeGroups, defaultNodePool or naas.
# Tier comes from the network: do not set spec.tier.
`,
  NetworkImport: (tenant) => `# Import an existing VPC read-only (cluster-vending network import -f). Review the
# discovered plan on the network's page, then take it over.
apiVersion: cluster-vending.io/v1
kind: NetworkImport
metadata:
  name: existing-network      # lowercase DNS label, 1-40 characters
  tenant: ${tenant}
  displayName: Existing VPC   # optional; labels are set after the takeover
spec:
  bootstrapId: 00000000-0000-0000-0000-000000000000   # REPLACE: the platform account ID; see References
  region: us-west-1
  aws:
    vpcId: vpc-0123456789abcdef0           # REPLACE with your VPC
    subnetIds:                             # REPLACE: 1-32 subnets of that VPC the platform will use
      - subnet-0123456789abcdef0
      - subnet-0123456789abcdef1
`,
  ClusterImport: (tenant) => `# Import an existing EKS cluster read-only (cluster-vending cluster import -f). Review the
# discovered plan on the cluster's page, then take it over.
apiVersion: cluster-vending.io/v1
kind: ClusterImport
metadata:
  name: existing-eks          # the existing EKS cluster's name
  tenant: ${tenant}
  displayName: Existing EKS   # optional; labels are set after the takeover
spec:
  networkId: 00000000-0000-0000-0000-000000000000   # REPLACE: the imported network's ID; see References
  aws:
    defaultNodeGroupName: existing-workers   # REPLACE: the node group that becomes the default pool
`,
};

export function manifestTemplate(kind: ManifestKind, tenantId: string): string {
  return templates[kind](tenantId);
}

/** The placeholder ID every template uses: review refuses it until it is replaced. */
export const PLACEHOLDER_ID = "00000000-0000-0000-0000-000000000000";
