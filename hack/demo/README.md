# End-to-end demo (vhs)

The demo records the CLI through everything a tenant does, and removes
everything it made. It is five phases, each its own tape and recording, so a
phase that fails is run again on its own, not the whole demo:

| Phase | Tape | `make` | Shows |
|---|---|---|---|
| 1 | `01-account.tape` | `demo-account` | `account create` onboards the account and prints how to create the platform's role (`cluster-vending-demo-<run>-provisioner`); the tape runs that `aws cloudformation create-stack`, then `account verify --wait` |
| 2 | `02-network.tape` | `demo-network` | `network create`: a VPC, two zones, one NAT gateway |
| 3 | `03-cluster.tape` | `demo-cluster` | `cluster create`: EKS on 1.36, alpha channel (1.37), upgrades automatic, tier 2 |
| 4 | `04-upgrade.tape` | `demo-upgrade` | the alpha rollout upgrades it to 1.37 by itself: `admin rollout get`, `cluster get`, `cluster events` |
| 5 | `05-cleanup.tape` | `demo-cleanup` | `cluster delete`, `network delete`, `account delete`, `aws cloudformation delete-stack` |

`make demo` runs them in order and stops at the first that fails. It is real:
it creates and deletes AWS resources (an IAM role, EKS, a NAT gateway) and
takes about 90 minutes; the waits are hidden, so each recording lasts a
minute or two.

## Run

```sh
# 1. The platform, locally, with automatic upgrades on:
#    hack/controller.local.yaml: automatic-upgrades: true
#    hack/config.local.yaml:     aws-management-role-arn: <the controller's role>
make run-controller        # one terminal
make run                   # another

# 2. Credentials of the demo account, in the shell that records (never typed on screen;
#    needed by the account and cleanup phases, which create and delete the role's stack):
export AWS_PROFILE=<profile>          # or AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
export DEMO_ACCOUNT_ID=<12-digit account ID>
# optional, fixed for a run once its first phase ran: DEMO_REGION (us-west-1),
# DEMO_ZONE_A (usw1-az1), DEMO_ZONE_B (usw1-az3), DEMO_TENANT (demo-<run>)

# 3. Record:
make demo                  # every phase: bin/demo/<run>-<phase>.gif and .mp4
make demo-video            # one bin/demo/<run>.mp4 of the phases (needs ffmpeg)
make demo-validate         # only parse the tapes
```

## A run and its state

Every run has its own names, from its id (`DEMO_RUN`, default the time,
`MMDDHHMM`): tenant and account `demo-<run>`, role and stack
`cluster-vending-demo-<run>-provisioner`, network `demo-<run>-net`, cluster
`demo-<run>-eks`. A run never touches another run's resources or anything
else in the account, so a new run can start while an older one is still
deleting.

What the phases learn (the run id, tenant, region and zones, the account,
network and cluster ids) is kept in `bin/demo/<run>.env`. A phase reads it,
so the phases of a run can be recorded at different times.

The run a phase acts on: `DEMO_RUN` when set; else the newest run whose
cleanup has not finished; else a new run.

```sh
make demo-status           # the run's state, which phases are done, and each piece now
```

## When a phase fails

The recording stops, and `make` prints why (also in
`bin/demo/<run>-<phase>-failed.txt`) and the command to run again, e.g.:

```
demo 01151430, cluster step 3 failed: the cluster demo-01151430-eks: ...
demo 01151430: cluster stopped; run it again with: make demo-cluster DEMO_RUN=01151430
```

Running a phase again resumes it: what an earlier attempt made is found by
its name (a create the platform refuses because it exists is not a failure),
and a phase whose result is there already (account verified, network READY,
cluster RUNNING, cluster on 1.37) is skipped. So after a fix, `make demo` or
the one phase picks up where the run stopped.

## Cleaning up a run

`make demo-cleanup` deletes the current run's cluster, network, then account
and the role's stack, skipping what is gone already. For another run, name
it: `make demo-cleanup DEMO_RUN=01151430`. By hand, in this order:

```sh
RUN=01151430 T=demo-$RUN
ID() { cluster-vending "$1" list --tenant "$T" -o json | jq -r ".$2[]? | select(.$3==\"$4\") | .id"; }

cluster-vending cluster delete --tenant $T --id "$(ID cluster clusters name demo-$RUN-eks)" --wait
cluster-vending network delete --tenant $T --id "$(ID network networks name demo-$RUN-net)" --wait
cluster-vending account delete --tenant $T --id "$(ID account accounts accountName demo-$RUN)" --wait
aws cloudformation delete-stack --stack-name cluster-vending-demo-$RUN-provisioner --region us-west-1
```

`account delete` deletes the account only: the role's stack is yours, so
the run deletes it with the AWS CLI.

## Tests

`go test ./pkg/cli -run TestDemo` checks the manifests the tapes fill (as
requests, without a server) and the run state and resume logic of `lib.sh`
and `state.sh` (with a fake `cluster-vending`).
