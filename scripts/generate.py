"""Generate the CloudFormation templates of the IAM roles cluster-vending
assumes in a tenant AWS account, and of the two roles of the platform's own
(management) account it runs as (scripts/management.py).

Run with `make cf-generate`. The templates are written to
pkg/cloudformation/templates/ and embedded in the cluster-vending binary,
which creates their stack in a tenant account (`cluster-vending account
create`). The role's name is a parameter (RoleName, default
cluster-vending-<role>), and its policies are named after it, so several
stacks of one role fit in one account.

Three roles, each a superset of the one before:

- reader: what the platform reads (import, the access check's reads, the
  Crossplane providers' observation). Read only.
- updater: reader, and changes to what exists (tags, attributes, versions)
  without creating or deleting anything.
- provisioner: updater, and creating and deleting the networks and EKS
  clusters the platform vends. The controller needs this one: it is the
  role an account is onboarded with today.

Every role trusts the platform's management role (the ManagementRoleArn
parameter), the identity the controller and the Crossplane providers run as,
and, when the ServerPrincipalArn parameter is not empty, the API server's
principal, which reads the cluster results buckets (read-only: the server
limits its session to that). With the ExternalId parameter (the account's,
which the API server generates at account create) a principal is trusted
only when it passes that external ID (sts:ExternalId), so another tenant's
account onboarded with the same management role cannot reach this one. The platform names its resources after the
network or cluster, without a common prefix, and adopts resources that it
did not create (import, take over), so mutations cannot be scoped by name
or by the platform's tags; IAM actions are limited by conditions where
AWS supports them (iam:PassedToService, iam:AWSServiceName,
kms:GrantIsForAWSResource).

Each managed policy stays under the 6,144 character limit of IAM managed
policies; main() checks it.
"""

import json
import os
import re
import sys

from awacs.aws import (
    Action,
    Allow,
    Bool,
    Condition,
    Null,
    PolicyDocument,
    Principal,
    Statement,
    StringEquals,
)
from awacs.sts import AssumeRole
from troposphere import (
    Equals,
    GetAtt,
    If,
    Not,
    Output,
    Parameter,
    Ref,
    Sub,
    Tags,
    Template,
)
from troposphere.iam import ManagedPolicy, Role, ServiceLinkedRole

import management

# The platform's name, on every role, policy and stack.
PLATFORM = "cluster-vending"

# The roles, in the order each adds to the one before.
ROLES = ["reader", "updater", "provisioner"]
PROVISIONER = "provisioner"


def template_version(root, constant="TemplateVersion"):
    """Return a version constant of pkg/cloudformation/version.go:
    TemplateVersion of the tenant templates, ManagementTemplateVersion of
    the management account's. The Go constant is the one source, bumped
    whenever a template changes (a test fails otherwise), and written into
    every template as a role tag and an output."""
    path = os.path.join(root, "pkg", "cloudformation", "version.go")
    with open(path) as file:
        match = re.search(rf"^const {constant} = (\d+)$", file.read(), re.M)
    if match is None:
        sys.exit(f"{path}: no line 'const {constant} = N'")
    return match.group(1)


# IAM's limit on a managed policy document, without whitespace.
MANAGED_POLICY_LIMIT = 6144

DESCRIPTIONS = {
    "reader": (
        "cluster-vending reader role: read-only access for the cluster-vending "
        "platform to the networks and EKS clusters of this account (import, the "
        "access check, observation). It cannot create, change or delete anything."
    ),
    "updater": (
        "cluster-vending updater role: the reader role, and changes to existing "
        "networks and EKS clusters (tags, attributes, versions). It cannot create "
        "or delete resources."
    ),
    "provisioner": (
        "cluster-vending provisioner role: the updater role, and creating and "
        "deleting the networks (VPC, subnets, gateways, routes, flow logs) and EKS "
        "clusters (node groups, add-ons, access, IAM roles, KMS keys, log groups, "
        "Karpenter queues) the cluster-vending platform manages in this account."
    ),
}


def actions(service, *names):
    """Return the awacs Actions of one service."""
    return [Action(service, name) for name in names]


def statement(sid, action_list, resource="*", condition=None):
    """Return one Allow statement; resource "*" unless named."""
    kwargs = {}
    if condition is not None:
        kwargs["Condition"] = condition
    return Statement(
        Sid=sid,
        Effect=Allow,
        Action=action_list,
        Resource=resource if isinstance(resource, list) else [resource],
        **kwargs,
    )


# --- reader ------------------------------------------------------------------

READ_NETWORK = [
    # Describe calls do not support resource-level permissions.
    statement(
        "ReadEC2",
        actions("ec2", "Describe*", "GetEbsEncryptionByDefault"),
    ),
]

READ_CLUSTERS = [
    statement("ReadEKS", actions("eks", "Describe*", "List*")),
    statement(
        "ReadIAM",
        actions(
            "iam",
            "GetRole",
            "GetRolePolicy",
            "GetPolicy",
            "GetPolicyVersion",
            "GetOpenIDConnectProvider",
            "ListRoles",
            "ListPolicies",
            "ListRolePolicies",
            "ListAttachedRolePolicies",
            "ListRoleTags",
            "ListPolicyTags",
            "ListPolicyVersions",
            "ListInstanceProfilesForRole",
            "ListOpenIDConnectProviders",
            "ListOpenIDConnectProviderTags",
        ),
    ),
    statement(
        "ReadKMS",
        actions(
            "kms",
            "DescribeKey",
            "GetKeyPolicy",
            "GetKeyRotationStatus",
            "ListAliases",
            "ListResourceTags",
        ),
    ),
    statement(
        "ReadLogs",
        actions(
            "logs",
            "DescribeLogGroups",
            "ListTagsForResource",
            "ListTagsLogGroup",
        ),
    ),
    # Karpenter's interruption queue and its EventBridge rules.
    statement(
        "ReadKarpenterEvents",
        actions("sqs", "GetQueueAttributes", "GetQueueUrl", "ListQueueTags")
        + actions("events", "DescribeRule", "ListTargetsByRule", "ListTagsForResource"),
    ),
]

# --- updater -----------------------------------------------------------------

UPDATE = [
    statement(
        "UpdateNetwork",
        actions(
            "ec2",
            "CreateTags",
            "DeleteTags",
            "ModifyVpcAttribute",
            "ModifySubnetAttribute",
            "ModifySecurityGroupRules",
            "UpdateSecurityGroupRuleDescriptionsEgress",
            "UpdateSecurityGroupRuleDescriptionsIngress",
            "ReplaceRoute",
            "ReplaceNetworkAclEntry",
            "ModifyLaunchTemplate",
            "CreateLaunchTemplateVersion",
        ),
    ),
    statement(
        "UpdateEKS",
        actions(
            "eks",
            "UpdateClusterConfig",
            "UpdateClusterVersion",
            "UpdateNodegroupConfig",
            "UpdateNodegroupVersion",
            "UpdateAddon",
            "UpdateAccessEntry",
            "UpdatePodIdentityAssociation",
            "TagResource",
            "UntagResource",
        ),
    ),
    statement(
        "UpdateIAM",
        actions(
            "iam",
            "TagRole",
            "UntagRole",
            "TagPolicy",
            "UntagPolicy",
            "TagOpenIDConnectProvider",
            "UntagOpenIDConnectProvider",
            "UpdateRole",
            "UpdateAssumeRolePolicy",
            "CreatePolicyVersion",
            "DeletePolicyVersion",
            "UpdateOpenIDConnectProviderThumbprint",
        ),
    ),
    statement(
        "UpdateKMSAndLogs",
        actions(
            "kms", "EnableKeyRotation", "TagResource", "UntagResource", "UpdateAlias"
        )
        + actions(
            "logs", "PutRetentionPolicy", "TagResource", "UntagResource", "TagLogGroup"
        ),
    ),
    statement(
        "UpdateKarpenterEvents",
        actions("sqs", "SetQueueAttributes", "TagQueue", "UntagQueue")
        + actions(
            "events", "PutTargets", "RemoveTargets", "TagResource", "UntagResource"
        ),
    ),
]

# --- provisioner -------------------------------------------------------------

PROVISION_NETWORK = [
    statement(
        "ProvisionNetwork",
        actions(
            "ec2",
            "CreateVpc",
            "DeleteVpc",
            "CreateSubnet",
            "DeleteSubnet",
            "CreateInternetGateway",
            "DeleteInternetGateway",
            "AttachInternetGateway",
            "DetachInternetGateway",
            "AllocateAddress",
            "ReleaseAddress",
            "DisassociateAddress",
            "CreateNatGateway",
            "DeleteNatGateway",
            "CreateRouteTable",
            "DeleteRouteTable",
            "CreateRoute",
            "DeleteRoute",
            "AssociateRouteTable",
            "DisassociateRouteTable",
            "ReplaceRouteTableAssociation",
            "CreateNetworkAcl",
            "DeleteNetworkAcl",
            "CreateNetworkAclEntry",
            "DeleteNetworkAclEntry",
            "ReplaceNetworkAclAssociation",
            "CreateSecurityGroup",
            "DeleteSecurityGroup",
            "AuthorizeSecurityGroupIngress",
            "AuthorizeSecurityGroupEgress",
            "RevokeSecurityGroupIngress",
            "RevokeSecurityGroupEgress",
            "CreateFlowLogs",
            "DeleteFlowLogs",
            "CreateLaunchTemplate",
            "DeleteLaunchTemplate",
            "DeleteLaunchTemplateVersions",
            # Node groups with a launch template start instances from it.
            "RunInstances",
        ),
    ),
]

PROVISION_CLUSTERS = [
    statement(
        "ProvisionEKS",
        actions(
            "eks",
            "CreateCluster",
            "DeleteCluster",
            "CreateNodegroup",
            "DeleteNodegroup",
            "CreateAddon",
            "DeleteAddon",
            "CreateAccessEntry",
            "DeleteAccessEntry",
            "AssociateAccessPolicy",
            "DisassociateAccessPolicy",
            "CreatePodIdentityAssociation",
            "DeletePodIdentityAssociation",
            "AssociateIdentityProviderConfig",
            "DisassociateIdentityProviderConfig",
        ),
    ),
    statement(
        "ProvisionIAM",
        actions(
            "iam",
            "CreateRole",
            "DeleteRole",
            "PutRolePolicy",
            "DeleteRolePolicy",
            "AttachRolePolicy",
            "DetachRolePolicy",
            "CreatePolicy",
            "DeletePolicy",
            "CreateOpenIDConnectProvider",
            "DeleteOpenIDConnectProvider",
        ),
    ),
    # Karpenter's instance profiles hold the node role, which cannot be
    # deleted while they do: the platform removes the ones Karpenter made
    # (tagged with its EC2NodeClass) for a cluster it deletes.
    statement(
        "ReadInstanceProfileTags",
        actions("iam", "ListInstanceProfileTags"),
    ),
    statement(
        "CleanKarpenterInstanceProfiles",
        actions("iam", "RemoveRoleFromInstanceProfile", "DeleteInstanceProfile"),
        condition=Condition(
            Null("aws:ResourceTag/karpenter.k8s.aws/ec2nodeclass", "false")
        ),
    ),
    # The cluster, node and component roles the platform creates are handed
    # only to the AWS services that use them.
    statement(
        "PassRolesToServices",
        actions("iam", "PassRole"),
        condition=Condition(
            StringEquals(
                "iam:PassedToService",
                [
                    "eks.amazonaws.com",
                    "ec2.amazonaws.com",
                    "pods.eks.amazonaws.com",
                    "vpc-flow-logs.amazonaws.com",
                ],
            )
        ),
    ),
    statement(
        "ServiceLinkedRoles",
        actions("iam", "CreateServiceLinkedRole"),
        condition=Condition(
            StringEquals(
                "iam:AWSServiceName",
                [
                    "eks.amazonaws.com",
                    "eks-nodegroup.amazonaws.com",
                    "autoscaling.amazonaws.com",
                ],
            )
        ),
    ),
    statement(
        "ProvisionKMS",
        actions(
            "kms",
            "CreateKey",
            "CreateAlias",
            "DeleteAlias",
            "PutKeyPolicy",
            "ScheduleKeyDeletion",
        ),
    ),
    # EKS secrets encryption: grants for the EKS service only.
    statement(
        "KMSGrantsForEKS",
        actions("kms", "CreateGrant"),
        condition=Condition(Bool("kms:GrantIsForAWSResource", "true")),
    ),
    statement(
        "ProvisionLogs",
        actions("logs", "CreateLogGroup", "DeleteLogGroup"),
    ),
    statement(
        "ProvisionKarpenterEvents",
        actions("sqs", "CreateQueue", "DeleteQueue")
        + actions("events", "PutRule", "DeleteRule"),
    ),
]

# The managed policies of each role: (name suffix, statements).
# The bucket of a cluster's scan results (docs/architecture.md#results-in-s3):
# the platform creates, configures and deletes buckets named cv-*, and reads,
# writes and deletes the objects in them. Nothing else of S3. The reads are
# what the Crossplane provider refreshes a bucket with.
RESULTS_BUCKETS = "arn:aws:s3:::cv-*"
RESULTS_OBJECTS = "arn:aws:s3:::cv-*/*"

PROVISION_RESULTS = [
    statement(
        "ProvisionResultsBuckets",
        actions(
            "s3",
            "CreateBucket",
            "DeleteBucket",
            "ListBucket",
            "PutBucketTagging",
            "PutBucketPublicAccessBlock",
            "PutBucketOwnershipControls",
            "DeleteBucketOwnershipControls",
            "PutEncryptionConfiguration",
            "PutBucketPolicy",
            "DeleteBucketPolicy",
            "PutLifecycleConfiguration",
            "GetBucket*",
            "GetEncryptionConfiguration",
            "GetLifecycleConfiguration",
            "GetAccelerateConfiguration",
            "GetReplicationConfiguration",
        ),
        RESULTS_BUCKETS,
    ),
    statement(
        "ReadWriteResultsObjects",
        actions("s3", "GetObject", "PutObject", "DeleteObject", "AbortMultipartUpload"),
        RESULTS_OBJECTS,
    ),
]

POLICIES = {
    "reader": [
        ("read-network", READ_NETWORK),
        ("read-clusters", READ_CLUSTERS),
    ],
    "updater": [("update", UPDATE)],
    "provisioner": [
        ("provision-network", PROVISION_NETWORK),
        ("provision-clusters", PROVISION_CLUSTERS),
        ("provision-results", PROVISION_RESULTS),
    ],
}


def policies_of(role):
    """Return the (name suffix, statements) of role and the roles below it."""
    out = []
    for r in ROLES[: ROLES.index(role) + 1]:
        out.extend(POLICIES[r])
    return out


def logical_id(suffix):
    """Return a CloudFormation logical ID: read-network -> ReadNetworkPolicy."""
    return "".join(part.capitalize() for part in suffix.split("-")) + "Policy"


def managed_policy(role, suffix, statements):
    """Return the managed policy of one part of a role."""
    document = PolicyDocument(Version="2012-10-17", Statement=statements)
    size = len(json.dumps(json.loads(document.to_json()), separators=(",", ":")))
    if size > MANAGED_POLICY_LIMIT:
        sys.exit(
            f"{role}/{suffix}: {size} characters, over IAM's {MANAGED_POLICY_LIMIT}"
        )
    return ManagedPolicy(
        logical_id(suffix),
        ManagedPolicyName=Sub("${RoleName}-" + suffix),
        Description=f"{PLATFORM} {role} role: {suffix.replace('-', ' ')}",
        PolicyDocument=document,
    )


def trust(principals, external_id):
    """Return the role's trust policy: the principals (parameters), passing
    the external ID when there is one."""
    statement = Statement(
        Effect=Allow,
        Action=[AssumeRole],
        Principal=Principal("AWS", [Ref(p) for p in principals]),
    )
    if external_id is not None:
        statement.Condition = Condition(
            StringEquals("sts:ExternalId", Ref(external_id))
        )
    return PolicyDocument(Version="2012-10-17", Statement=[statement])


def add_spot_service_linked_role(template):
    """Add the account's EC2 Spot service-linked role, on request.

    Karpenter's Spot nodes need AWSServiceRoleForEC2Spot. CloudFormation
    cannot create a resource that already exists, and an account that ever
    used Spot has this role, so the stack creates it only when the
    CreateSpotServiceLinkedRole parameter says "true" (off by default: no
    stack update of an existing account can fail on it). The role is the
    account's, not the stack's, so it is retained when the stack is deleted.
    """
    parameter = template.add_parameter(
        Parameter(
            "CreateSpotServiceLinkedRole",
            Type="String",
            Default="false",
            AllowedValues=["true", "false"],
            Description=(
                "Create the account's EC2 Spot service-linked role "
                "(AWSServiceRoleForEC2Spot), which Karpenter's Spot nodes need. "
                "Set true only when the account does not have it yet "
                "(aws iam get-role --role-name AWSServiceRoleForEC2Spot): "
                "CloudFormation fails to create a role that exists."
            ),
        )
    )
    template.add_condition(
        "CreateSpotServiceLinkedRole", Equals(Ref(parameter), "true")
    )
    template.add_resource(
        ServiceLinkedRole(
            "SpotServiceLinkedRole",
            AWSServiceName="spot.amazonaws.com",
            Description="EC2 Spot, for the platform's Karpenter nodes",
            Condition="CreateSpotServiceLinkedRole",
            DeletionPolicy="Retain",
            UpdateReplacePolicy="Retain",
        )
    )


def template_of(role, version):
    """Return the CloudFormation template of one role."""
    template = Template()
    template.set_version("2010-09-09")
    template.set_description(DESCRIPTIONS[role])
    management_role = template.add_parameter(
        Parameter(
            "ManagementRoleArn",
            Type="String",
            Description=(
                "ARN of the cluster-vending management role (the identity the "
                "controller and the Crossplane providers run as); the role trusts "
                "only it. cluster-vending account create fills it in from the "
                "API server."
            ),
            AllowedPattern=r"^arn:aws[a-z-]*:iam::\d{12}:role/.+$",
        )
    )
    role_name = template.add_parameter(
        Parameter(
            "RoleName",
            Type="String",
            Default=f"{PLATFORM}-{role}",
            Description=(
                f"Name of the IAM role, and prefix of its policies: {PLATFORM}-{role}, "
                f"or {PLATFORM}-<anything>-{role} for a stack of its own."
            ),
            MaxLength=64,
            AllowedPattern=rf"^{PLATFORM}-([A-Za-z0-9+=,.@_-]+-)?{role}$",
        )
    )
    external_id = template.add_parameter(
        Parameter(
            "ExternalId",
            Type="String",
            Default="",
            NoEcho=True,
            Description=(
                "The account's external ID (cluster-vending account create fills "
                "it in): the management role must pass it to assume this role. "
                "Empty: none is required (accounts onboarded before external IDs)."
            ),
            MaxLength=1224,
            AllowedPattern=r"^$|^[A-Za-z0-9+=,.@:/_-]{32,1224}$",
        )
    )
    server_principal = template.add_parameter(
        Parameter(
            "ServerPrincipalArn",
            Type="String",
            Default="",
            Description=(
                "ARN of the role the cluster-vending API server runs as, which "
                "reads the cluster results buckets (read-only). cluster-vending "
                "account create fills it in from the API server. Empty: the "
                "server has no AWS access, and only the management role is trusted."
            ),
            AllowedPattern=r"^$|^arn:aws[a-z-]*:iam::\d{12}:role/.+$",
        )
    )
    template.add_condition("HasExternalId", Not(Equals(Ref(external_id), "")))
    template.add_condition(
        "HasServerPrincipalArn", Not(Equals(Ref(server_principal), ""))
    )
    if role == PROVISIONER:
        add_spot_service_linked_role(template)
    refs = [
        Ref(template.add_resource(managed_policy(role, suffix, statements)))
        for suffix, statements in policies_of(role)
    ]
    iam_role = template.add_resource(
        Role(
            "Role",
            RoleName=Ref(role_name),
            Description=f"Assumed by the {PLATFORM} platform ({role})",
            AssumeRolePolicyDocument=If(
                "HasExternalId",
                If(
                    "HasServerPrincipalArn",
                    trust([management_role, server_principal], external_id),
                    trust([management_role], external_id),
                ),
                If(
                    "HasServerPrincipalArn",
                    trust([management_role, server_principal], None),
                    trust([management_role], None),
                ),
            ),
            ManagedPolicyArns=refs,
            Tags=Tags(
                {
                    "managed-by": PLATFORM,
                    f"{PLATFORM}.io/role": role,
                    f"{PLATFORM}.io/template-version": version,
                }
            ),
        )
    )
    template.add_output(
        Output(
            "RoleArn",
            Description="The role's ARN",
            Value=GetAtt(iam_role, "Arn"),
        )
    )
    template.add_output(
        Output(
            "RoleName",
            Description="The role's name: spec.aws.role of the CloudAccount",
            Value=Ref(iam_role),
        )
    )
    template.add_output(
        Output(
            "TemplateVersion",
            Description=(
                "The version of this template: the platform compares it with "
                "what it needs and says when the stack is to be updated"
            ),
            Value=version,
        )
    )
    return template


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    out_dir = os.path.join(root, "pkg", "cloudformation", "templates")
    os.makedirs(out_dir, exist_ok=True)
    version = template_version(root)
    for role in ROLES:
        path = os.path.join(out_dir, f"{PLATFORM}-{role}-role.template.yaml")
        with open(path, "w") as file:
            file.write(template_of(role, version).to_yaml())
    version = template_version(root, "ManagementTemplateVersion")
    for role in management.ROLES:
        path = os.path.join(out_dir, f"{PLATFORM}-{role}-role.template.yaml")
        with open(path, "w") as file:
            file.write(management.template_of(role, version).to_yaml())


if __name__ == "__main__":
    main()
