# End-to-end demo (vhs)

`demo.tape` records the CLI through everything a tenant does, and removes
everything it made:

1. `account create`: the CLI creates the platform's IAM role in the AWS
   account with CloudFormation (its own role, `cluster-vending-demo-<run>-provisioner`),
   onboards the account, and the controller verifies its access;
2. `network create`: a VPC with two zones and one NAT gateway;
3. `cluster create`: EKS on Kubernetes 1.36, on the alpha channel (1.37),
   upgrades automatic, tier 2;
4. the alpha rollout upgrades it to 1.37 (control plane, then node groups and
   add-ons): `admin rollout get`, `cluster get`, `cluster events`;
5. `cluster delete`, `network delete`, then `account delete --delete-role`:
   the account, and the role's stack.

It is real: it creates and deletes AWS resources (an IAM role, EKS, a NAT
gateway) and takes about 90 minutes. The waits are hidden, so the recording
lasts a few minutes. A step that fails stops the recording, and `make demo`
prints why (`bin/demo-failed.txt`).

## Each run is independent

Every run has its own names, from `DEMO_RUN` (default: the time, `MMDDHHMM`):

| What | Name |
|---|---|
| tenant | `demo-<run>` (or `DEMO_TENANT`) |
| account | `demo-<run>` |
| IAM role and its stack | `cluster-vending-demo-<run>-provisioner` |
| network | `demo-<run>-net` |
| cluster | `demo-<run>-eks` |

So a new run can start while an earlier one is still deleting, and a run
never touches another run's resources or anything else in the account (an
existing `cluster-vending-provisioner` role, other tenants).

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
# optional: DEMO_RUN, DEMO_REGION (us-west-1), DEMO_ZONE_A (usw1-az1), DEMO_ZONE_B (usw1-az3)

# 3. Record:
make demo                  # bin/demo.gif and bin/demo.mp4
make demo-validate         # only parse the tape
```

`go test ./pkg/cli -run TestDemoManifestsAreValid` checks the manifests the
tape fills, without a server.

## Cleaning up a run that stopped

With the run's id (its names above, e.g. `demo-01151430-eks`), delete in
this order; each waits until the resource is gone:

```sh
RUN=01151430 T=demo-$RUN
ID() { cluster-vending "$1" list --tenant "$T" -o json | jq -r ".$2[]? | select(.$3==\"$4\") | .id"; }

cluster-vending cluster delete --tenant $T --id "$(ID cluster clusters name demo-$RUN-eks)" --wait
cluster-vending network delete --tenant $T --id "$(ID network networks name demo-$RUN-net)" --wait
cluster-vending account delete --tenant $T --id "$(ID account accounts accountName demo-$RUN)" \
  --delete-role --region us-west-1 --wait
```

`account delete --delete-role` deletes the role's stack only when `account
create` made it (its `cluster-vending.io/managed-by` tag), and refuses any
other before deleting anything.
