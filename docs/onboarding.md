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

## Onboarding the account

One command creates the role and onboards the account. The manifest has
everything it needs: the account and the role.

```yaml
# cloudaccount.yaml
apiVersion: cluster-vending.io/v1
kind: CloudAccount
metadata: {name: acme-prod, tenant: acme}
spec:
  aws:
    accountId: "444455556666"
    role: cluster-vending-provisioner   # reader, updater or provisioner
```

```sh
cluster-vending account create -f cloudaccount.yaml [--create-role terminal|browser|skip] [--profile <aws profile>] [--region us-east-1]
```

1. **The role's stack.** When `spec.aws.role` is one of the platform's
   (`cluster-vending-reader`, `-updater`, `-provisioner`), the CLI gets its
   template and the management role from the API server
   (`GetAccountOnboarding`) and creates the stack `cluster-vending-<role>`
   in the account. How is `--create-role`; at a terminal without it, that
   is the one question asked:
   - `terminal`: here, with your AWS credentials of the account (the AWS
     SDK's default chain: environment, `~/.aws` profiles, SSO; `--profile`
     picks one). The credentials must be of the manifest's account, or
     nothing is created. A stack that already has the role's template and
     management role is left alone; one with an older template is updated.
     Each stack event is printed as it happens.
   - `browser`: opens the CloudFormation console's quick-create link (and
     prints it, for a session without a browser). Sign in to the account,
     acknowledge that the stack creates IAM resources and choose **Create
     stack**. With credentials of the account here, the CLI follows the
     stack until it is created; without, it goes on and the access check
     tells whether the role works. The link needs the templates on S3
     (`cloudformation-template-url`, below); without it the CLI creates the
     stack from the terminal instead.
   - `skip`: the role exists already.

   A stack that fails stops the command, with the reasons of the events
   that failed: the account is not onboarded. Not at a terminal (CI),
   `--create-role` is required. A role that is not one of the platform's is
   yours to create, trusting the management role; no stack is created.
2. **The account** is onboarded: its namespace and ProviderConfig
   (`BootstrapTenant`).
3. **The access check.** With `--wait` (the default) the CLI asks the
   controller to check the account (`VerifyCloudAccount`) and waits for its
   verdict: `Access: verified`, or the check that failed and a non-zero
   exit. `--wait=false` returns once the account is onboarded; then:

```sh
cluster-vending account verify --id <account id> --tenant acme
cluster-vending account get --id <account id> --tenant acme   # ACCESS: Verified
```

Networks and clusters are refused until the access is verified.

The API server also serves the templates for download, without a token (a
template holds no secret):
`<server>/cloudformation/cluster-vending-provisioner-role.template.yaml`.

## Server settings

| Setting | |
|---|---|
| `aws-management-role-arn` | The role the templates trust: the controller's setting of the same name. Without it `account create` cannot create a stack: `GetAccountOnboarding` fails with `FAILED_PRECONDITION`, saying so. The server makes no AWS call with it. |
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
