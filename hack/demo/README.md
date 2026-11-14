# End-to-end demo (vhs)

`demo.tape` records the CLI from onboarding an AWS account to an automatic
Kubernetes upgrade, and deletes what it created:

1. `account create` creates the platform's role with CloudFormation (only if
   the account has no `cluster-vending-provisioner` role yet) and checks access;
2. `network create`: a VPC with two zones and one NAT gateway (`demo-net`);
3. `cluster create`: EKS `demo-eks` on Kubernetes 1.36, on the alpha channel
   (1.37), upgrades automatic, tier 2;
4. the alpha rollout upgrades it to 1.37 (control plane, then node groups and
   add-ons): `admin rollout get`, `cluster get`, `cluster events`;
5. `cluster delete`, `network delete`, and the role's stack if the demo
   created it.

It is real: it creates and deletes AWS resources (EKS, NAT gateway) and takes
about 90 minutes. The waits are hidden, so the recording lasts a few minutes.
It uses its own tenant (`demo`) and names; nothing else is touched.

## Run

```sh
# 1. The platform, locally, with automatic upgrades on:
#    hack/controller.local.yaml: automatic-upgrades: true
#    hack/config.local.yaml:     aws-management-role-arn: <the controller's role>
make run-controller        # one terminal
make run                   # another

# 2. Credentials of the demo account, in the shell that runs vhs (never typed on screen):
export AWS_PROFILE=<profile>          # or AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
export DEMO_ACCOUNT_ID=<12-digit account ID>
# optional: DEMO_REGION (us-west-1), DEMO_ZONE_A (usw1-az1), DEMO_ZONE_B (usw1-az3)

# 3. Record:
make demo                  # bin/demo.gif and bin/demo.mp4
make demo-validate         # only parse the tape
```

The account record stays: the CLI has no `account delete` yet.
