"""The CloudFormation templates of the IAM roles of the platform's own
(management) AWS account, which scripts/generate.py writes next to the
tenant role templates.

Two roles, created once per management account, before any tenant account:

- management: the identity the controller and the Crossplane AWS providers
  run as (the controller's and the server's aws-management-role-arn, and the
  ManagementRoleArn every tenant role trusts).
- server: the identity the API server runs as (aws-server-role-arn, and the
  ServerPrincipalArn tenant roles trust), which reads the cluster results
  buckets through the tenant roles (the server limits that session to
  reads, results.ReadOnlySessionPolicy).

Each is trusted through IRSA by the exact service accounts that run as it in
the management EKS cluster (OidcProviderArn), by a local principal
(LocalPrincipalArn: an IAM user or role, for local development), or both;
a CloudFormation rule requires at least one. Each may do one thing: assume
the tenant roles (sts:AssumeRole on arn:<partition>:iam::*:role/
<TenantRolePattern>). The platform makes no other AWS call with these
identities (sts:GetCallerIdentity needs no permission), and passes no
session tags, so there is no sts:TagSession.
"""

import json

from awacs.aws import Allow, PolicyDocument, Principal, Statement
from awacs.sts import AssumeRole
from troposphere import (
    And,
    Condition,
    Equals,
    GetAtt,
    If,
    Not,
    Or,
    Output,
    Parameter,
    Ref,
    Select,
    Split,
    Sub,
    Tags,
    Template,
)
from troposphere.iam import Policy, Role

PLATFORM = "cluster-vending"

# The roles of the management account, in the order they are created.
ROLES = ["management", "server"]

# The service accounts that run as each role, in the management cluster.
# The platform's own are parameters (ServiceAccount), defaulting to the Helm
# chart's names (charts/cluster-vending: <fullname>-controller and
# <fullname>-server, fullname cluster-vending for the documented release);
# config/default names the controller's cluster-vending-controller-manager.
# Crossplane names a provider's service account after its package revision,
# provider-aws-<service>-<12 hex digits>, so the providers' is a StringLike
# pattern; provider-helm and the family provider make no AWS call and do
# not match it.
SERVICE_ACCOUNTS = {
    "management": f"{PLATFORM}-controller",
    "server": f"{PLATFORM}-server",
}
PROVIDERS_SA = "provider-aws-*"

DESCRIPTIONS = {
    "management": (
        "cluster-vending management role: the identity the cluster-vending "
        "controller and the Crossplane AWS providers run as (IRSA, or a local "
        "principal). It may only assume the tenant roles."
    ),
    "server": (
        "cluster-vending server role: the identity the cluster-vending API "
        "server runs as (IRSA, or a local principal), which reads the cluster "
        "results buckets through the tenant roles. It may only assume them."
    ),
}

# The output each role's ARN is under, named after the setting it goes in.
ARN_OUTPUTS = {
    "management": (
        "ManagementRoleArn",
        "aws-management-role-arn of the controller and the API server, and "
        "ManagementRoleArn of every tenant role stack",
    ),
    "server": (
        "ServerRoleArn",
        "aws-server-role-arn of the API server, and ServerPrincipalArn of "
        "every tenant role stack",
    ),
}

# An IAM role name.
ROLE_NAME_PATTERN = r"^[\w+=,.@-]{1,64}$"
# The OIDC provider IAM made for the management EKS cluster's issuer.
OIDC_PROVIDER_PATTERN = r"^$|^arn:aws[a-z-]*:iam::\d{12}:oidc-provider/.+$"
# An IAM user or role, the principal of a local run.
LOCAL_PRINCIPAL_PATTERN = r"^$|^arn:aws[a-z-]*:iam::\d{12}:(user|role)/.+$"
# A Kubernetes namespace, and a service account name (a DNS subdomain).
NAMESPACE_PATTERN = r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"
SERVICE_ACCOUNT_PATTERN = r"^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$"
# The tenant roles' names, an IAM resource pattern (* and ? wildcards).
TENANT_ROLE_PATTERN = r"^[\w+=,.@*?/-]{1,128}$"

# IAM's session limits of a role: one hour to twelve.
MIN_SESSION, MAX_SESSION = 3600, 43200

# Appended to the OIDC provider ARN before it is split into the issuer
# (arn:...:oidc-provider/<issuer>:oidc-provider/none -> <issuer>): with no
# provider, ":oidc-provider/none" still splits in two, so Fn::Select cannot
# fail, even in the Fn::If branch that is not taken.
NO_OIDC_PROVIDER = ":oidc-provider/none"


def parameters(template, role):
    """Add the role's parameters and conditions; return them by name."""
    p = {}
    p["RoleName"] = template.add_parameter(
        Parameter(
            "RoleName",
            Type="String",
            Default=f"{PLATFORM}-{role}",
            MaxLength=64,
            AllowedPattern=ROLE_NAME_PATTERN,
            Description="Name of the IAM role, and prefix of its policy.",
        )
    )
    p["OidcProviderArn"] = template.add_parameter(
        Parameter(
            "OidcProviderArn",
            Type="String",
            Default="",
            AllowedPattern=OIDC_PROVIDER_PATTERN,
            Description=(
                "ARN of the IAM OIDC provider of the management EKS cluster "
                "(production, IRSA): the role trusts the service accounts that "
                "run as it there. Empty: no IRSA (local development)."
            ),
        )
    )
    p["PlatformNamespace"] = template.add_parameter(
        Parameter(
            "PlatformNamespace",
            Type="String",
            Default=f"{PLATFORM}-system",
            AllowedPattern=NAMESPACE_PATTERN,
            Description=(
                f"Namespace of the {PLATFORM} controller and API server in the "
                "management cluster (with IRSA)."
            ),
        )
    )
    p["ServiceAccount"] = template.add_parameter(
        Parameter(
            "ServiceAccount",
            Type="String",
            Default=SERVICE_ACCOUNTS[role],
            AllowedPattern=SERVICE_ACCOUNT_PATTERN,
            Description=(
                f"Service account of the {PLATFORM} "
                + ("controller" if role == "management" else "API server")
                + " in PlatformNamespace (with IRSA): the Helm chart's "
                f"{SERVICE_ACCOUNTS[role]} by default"
                + (
                    f"; {PLATFORM}-controller-manager with config/default."
                    if role == "management"
                    else "."
                )
            ),
        )
    )
    if role == "management":
        p["CrossplaneNamespace"] = template.add_parameter(
            Parameter(
                "CrossplaneNamespace",
                Type="String",
                Default="crossplane-system",
                AllowedPattern=NAMESPACE_PATTERN,
                Description=(
                    "Namespace of Crossplane, whose AWS providers run as this "
                    "role (with IRSA)."
                ),
            )
        )
    p["LocalPrincipalArn"] = template.add_parameter(
        Parameter(
            "LocalPrincipalArn",
            Type="String",
            Default="",
            AllowedPattern=LOCAL_PRINCIPAL_PATTERN,
            Description=(
                "ARN of an IAM user or role that may assume this role "
                "(local development: aws sts get-caller-identity; an SSO "
                "session's role, not its assumed-role ARN). Empty: none."
            ),
        )
    )
    p["TenantRolePattern"] = template.add_parameter(
        Parameter(
            "TenantRolePattern",
            Type="String",
            Default=f"{PLATFORM}-*",
            AllowedPattern=TENANT_ROLE_PATTERN,
            Description=(
                "Names of the tenant roles this role may assume, in any "
                f"account: {PLATFORM}-* covers every template's role (reader, "
                "updater, provisioner)."
            ),
        )
    )
    p["MaxSessionDuration"] = template.add_parameter(
        Parameter(
            "MaxSessionDuration",
            Type="Number",
            Default=MIN_SESSION,
            MinValue=MIN_SESSION,
            MaxValue=MAX_SESSION,
            Description=(
                "Longest session of this role, in seconds (3600 to 43200): "
                "up to 43200 for a long local session; IRSA renews by itself."
            ),
        )
    )
    template.add_condition(
        "HasOidcProvider", Not(Equals(Ref(p["OidcProviderArn"]), ""))
    )
    template.add_condition(
        "HasLocalPrincipal", Not(Equals(Ref(p["LocalPrincipalArn"]), ""))
    )
    template.add_condition(
        "HasBoth", And(Condition("HasOidcProvider"), Condition("HasLocalPrincipal"))
    )
    template.add_rule(
        "TrustSource",
        {
            "Assertions": [
                {
                    "Assert": Or(
                        Not(Equals(Ref(p["OidcProviderArn"]), "")),
                        Not(Equals(Ref(p["LocalPrincipalArn"]), "")),
                    ),
                    "AssertDescription": (
                        "Set OidcProviderArn (IRSA), LocalPrincipalArn (local), "
                        "or both: the role must trust someone."
                    ),
                }
            ]
        },
    )
    return p


def irsa_statement(sid, namespace_parameter, service_account):
    """Return the IRSA trust of one service account (a ${parameter}, or a
    pattern with *) as a JSON-ready dict with ${...} placeholders for
    Fn::Sub: the condition keys hold the issuer, which CloudFormation cannot
    put in an object key otherwise."""
    subject = f"system:serviceaccount:${{{namespace_parameter}}}:{service_account}"
    conditions = {"StringEquals": {"${Issuer}:aud": "sts.amazonaws.com"}}
    if service_account.endswith("*"):
        conditions["StringLike"] = {"${Issuer}:sub": subject}
    else:
        conditions["StringEquals"]["${Issuer}:sub"] = subject
    return {
        "Sid": sid,
        "Effect": "Allow",
        "Principal": {"Federated": "${OidcProviderArn}"},
        "Action": "sts:AssumeRoleWithWebIdentity",
        "Condition": conditions,
    }


LOCAL_STATEMENT = {
    "Sid": "LocalPrincipal",
    "Effect": "Allow",
    "Principal": {"AWS": "${LocalPrincipalArn}"},
    "Action": "sts:AssumeRole",
}


def irsa_statements(role):
    """Return the IRSA trust statements of the role's service accounts."""
    if role == "management":
        return [
            irsa_statement("ControllerIRSA", "PlatformNamespace", "${ServiceAccount}"),
            irsa_statement(
                "CrossplaneProvidersIRSA", "CrossplaneNamespace", PROVIDERS_SA
            ),
        ]
    return [irsa_statement("ServerIRSA", "PlatformNamespace", "${ServiceAccount}")]


def trust_document(statements, p):
    """Return the trust policy of these statements: an Fn::Sub of its JSON,
    with the issuer of the OIDC provider ARN as ${Issuer}."""
    document = {"Version": "2012-10-17", "Statement": statements}
    issuer = Select(
        1, Split(":oidc-provider/", Sub("${OidcProviderArn}" + NO_OIDC_PROVIDER))
    )
    return Sub(json.dumps(document, indent=2), Issuer=issuer)


def trust(role, p):
    """Return the role's trust policy: IRSA, the local principal, or both."""
    irsa = irsa_statements(role)
    return If(
        "HasBoth",
        trust_document(irsa + [LOCAL_STATEMENT], p),
        If(
            "HasOidcProvider",
            trust_document(irsa, p),
            PolicyDocument(
                Version="2012-10-17",
                Statement=[
                    Statement(
                        Sid="LocalPrincipal",
                        Effect=Allow,
                        Action=[AssumeRole],
                        Principal=Principal("AWS", [Ref(p["LocalPrincipalArn"])]),
                    )
                ],
            ),
        ),
    )


def assume_tenant_roles(p):
    """Return the role's only permission: assuming the tenant roles."""
    return PolicyDocument(
        Version="2012-10-17",
        Statement=[
            Statement(
                Sid="AssumeTenantRoles",
                Effect=Allow,
                Action=[AssumeRole],
                Resource=[
                    Sub("arn:${AWS::Partition}:iam::*:role/${TenantRolePattern}")
                ],
            )
        ],
    )


def template_of(role, version):
    """Return the CloudFormation template of one management account role."""
    template = Template()
    template.set_version("2010-09-09")
    template.set_description(DESCRIPTIONS[role])
    p = parameters(template, role)
    iam_role = template.add_resource(
        Role(
            "Role",
            RoleName=Ref(p["RoleName"]),
            Description=f"The {PLATFORM} platform's {role} role",
            MaxSessionDuration=Ref(p["MaxSessionDuration"]),
            AssumeRolePolicyDocument=trust(role, p),
            Policies=[
                Policy(
                    PolicyName=Sub("${RoleName}-assume-tenant-roles"),
                    PolicyDocument=assume_tenant_roles(p),
                )
            ],
            Tags=Tags(
                {
                    "managed-by": PLATFORM,
                    f"{PLATFORM}.io/role": role,
                    f"{PLATFORM}.io/template-version": version,
                }
            ),
        )
    )
    name, description = ARN_OUTPUTS[role]
    template.add_output(
        Output(name, Description=description, Value=GetAtt(iam_role, "Arn"))
    )
    template.add_output(
        Output("RoleName", Description="The role's name", Value=Ref(iam_role))
    )
    template.add_output(
        Output(
            "TemplateVersion",
            Description=(
                "The version of this template (ManagementTemplateVersion of "
                "pkg/cloudformation/version.go)"
            ),
            Value=version,
        )
    )
    return template
