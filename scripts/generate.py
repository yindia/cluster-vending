"""Generate the CloudFormation templates of the IAM roles cluster-vending
assumes in a tenant AWS account.

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

Every role trusts one principal: the platform's management role (the
ManagementRoleArn parameter), the identity the controller and the
Crossplane providers run as. The platform names its resources after the
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
import sys

from awacs.aws import (
    Action,
    Allow,
    Bool,
    Condition,
    PolicyDocument,
    Principal,
    Statement,
    StringEquals,
)
from awacs.sts import AssumeRole
from troposphere import GetAtt, Output, Parameter, Ref, Sub, Tags, Template
from troposphere.iam import ManagedPolicy, Role

# The platform's name, on every role, policy and stack.
PLATFORM = "cluster-vending"

# The roles, in the order each adds to the one before.
ROLES = ["reader", "updater", "provisioner"]

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
POLICIES = {
    "reader": [
        ("read-network", READ_NETWORK),
        ("read-clusters", READ_CLUSTERS),
    ],
    "updater": [("update", UPDATE)],
    "provisioner": [
        ("provision-network", PROVISION_NETWORK),
        ("provision-clusters", PROVISION_CLUSTERS),
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


def template_of(role):
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
    refs = [
        Ref(template.add_resource(managed_policy(role, suffix, statements)))
        for suffix, statements in policies_of(role)
    ]
    iam_role = template.add_resource(
        Role(
            "Role",
            RoleName=Ref(role_name),
            Description=f"Assumed by the {PLATFORM} platform ({role})",
            AssumeRolePolicyDocument=PolicyDocument(
                Version="2012-10-17",
                Statement=[
                    Statement(
                        Effect=Allow,
                        Action=[AssumeRole],
                        Principal=Principal("AWS", Ref(management_role)),
                    )
                ],
            ),
            ManagedPolicyArns=refs,
            Tags=Tags({"managed-by": PLATFORM, f"{PLATFORM}.io/role": role}),
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
    return template


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    out_dir = os.path.join(root, "pkg", "cloudformation", "templates")
    os.makedirs(out_dir, exist_ok=True)
    for role in ROLES:
        path = os.path.join(out_dir, f"{PLATFORM}-{role}-role.template.yaml")
        with open(path, "w") as file:
            file.write(template_of(role).to_yaml())


if __name__ == "__main__":
    main()
