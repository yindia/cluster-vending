# Onboarding an AWS account

The platform works in a tenant's AWS account through one IAM role there,
assumed from its management role (the identity the controller and the
Crossplane providers run as). The tenant creates that role with a
CloudFormation stack the platform provides, then onboards the account with
the role's name.

## The roles

Three templates, each a superset of the one before. Role names are
`cluster-vending-<role>`; each policy is `cluster-vending-<role>-<part>`.

| Role | What it may do | Use |
|---|---|---|
| `reader` | `ec2:Describe*`; EKS `Describe*` and `List*`; IAM `Get*`/`List*` of roles, policies and OIDC providers; KMS, CloudWatch Logs, SQS and EventBridge reads | imports and the providers' observation only: the access check reports `ec2:CreateVpc` as not allowed, so no network or cluster is created |
| `updater` | `reader`, plus changes to what exists: tags; VPC, subnet and security group rule attributes; routes and NACL entries; launch template versions; EKS cluster, node group, add-on, access entry and pod identity updates; IAM role, trust policy and policy versions; KMS key rotation and aliases; log retention; Karpenter queue attributes and targets | changing what exists, without creating or deleting anything |
| `provisioner` | `updater`, plus creating and deleting VPCs, subnets, internet and NAT gateways, Elastic IPs, route tables and routes, NACLs, security groups, flow logs and launch templates; EKS clusters, node groups, add-ons, access entries and policies, pod identity associations and OIDC identity providers; IAM roles, inline and managed policies and OIDC providers; KMS keys and aliases; log groups; Karpenter's SQS queue and EventBridge rules | **what the controller needs today**: it creates and deletes networks and clusters |

Conditions limit what AWS allows to be limited:

- `iam:PassRole` only to `eks.amazonaws.com`, `ec2.amazonaws.com`,
  `pods.eks.amazonaws.com` and `vpc-flow-logs.amazonaws.com`.
- `iam:CreateServiceLinkedRole` only for `eks`, `eks-nodegroup` and
  `autoscaling`.
- `kms:CreateGrant` only for AWS resources (EKS secrets encryption).

Resources are `*`: the platform names what it creates after the network
or cluster, with no common prefix, and adopts resources it did not create
(import, take over), so neither names nor the platform's tags can scope
them. `provisioner` can create IAM roles and attach policies to them, which
the cluster and component roles need; give it to the platform only, in
accounts meant for it, and keep service control policies as the outer
bound.

Each role trusts exactly one principal: the template's `ManagementRoleArn`
parameter. There is no external ID yet: the platform assumes the role
without `sts:ExternalId`, and a template requiring one would lock it out.

## Creating the role

```sh
# The template of the role (default provisioner), and how to create it:
cluster-vending account template --tenant acme --account-id 444455556666 [--role provisioner] [--region us-east-1] [--out role.yaml]
```

It writes the template to `--out` (or a new file in the temporary
directory) and prints:

- the AWS CLI command, to run with credentials of the tenant account:
  ```sh
  aws cloudformation create-stack --stack-name cluster-vending-provisioner \
    --template-body file://role.yaml --capabilities CAPABILITY_NAMED_IAM \
    --parameters ParameterKey=ManagementRoleArn,ParameterValue=<management role> --region us-east-1
  ```
- where the API server serves the template:
  `<server>/cloudformation/cluster-vending-provisioner-role.template.yaml`
  (no token needed: a template holds no secret);
- a CloudFormation console quick-create link, when the server has
  `cloudformation-template-url` (below);
- what to run next: `account create` with `spec.aws.role:
  cluster-vending-provisioner`, then `account verify`.

`account create` prints the same next steps, and `account template --id
<account id>` uses an onboarded account's role and AWS account.

Then:

```sh
cluster-vending account create -f cloudaccount.yaml          # spec.aws: {accountId: ..., role: cluster-vending-provisioner}
cluster-vending account verify --id <account id> --tenant acme
cluster-vending account get --id <account id> --tenant acme   # ACCESS: Verified
```

## Server settings

| Setting | |
|---|---|
| `aws-management-role-arn` | The role the templates trust: the controller's setting of the same name. Without it `account template` fails with `FAILED_PRECONDITION`, saying so. The server makes no AWS call with it. |
| `cloudformation-template-url` | Optional: an `https` location the templates are uploaded to, by file name. CloudFormation reads a template URL only from S3, so for the console link upload `pkg/cloudformation/templates/` to a bucket the tenant accounts can read and set this to it (for example `https://<bucket>.s3.amazonaws.com/cluster-vending`). The server's own `/cloudformation/` URL is for downloading: the console cannot use it. |

## Changing the templates

`scripts/generate.py` (troposphere and awacs) generates them into
`pkg/cloudformation/templates/`, which the binary embeds; do not edit the
YAML by hand. With Python 3.14 (pinned with pip-compile on 3.14; in the
workspace's pixi environment run `pixi shell` first) or a virtualenv:

```sh
make cf-requirements   # pip install -r requirements.txt (pinned; requirements.in lists the direct ones)
make cf-generate       # scripts/generate.py: writes the three templates
make cf-lint           # cfn-lint over them
make cf-format-check   # black --check scripts/generate.py
```

The generator refuses a managed policy over IAM's 6,144 character limit.
Commit the regenerated templates with the script.
