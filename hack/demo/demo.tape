# cluster-vending demo, end to end: onboard an AWS account (the CLI creates
# the platform's role with CloudFormation), create a network and an EKS
# cluster on Kubernetes 1.36, watch the alpha channel upgrade it to 1.37 by
# itself, then delete everything: the cluster, the network, the account and
# its role.
#
#   make demo            # writes bin/demo.gif (and bin/demo.mp4)
#
# Before running (see hack/demo/README.md):
#   - the API server and the controller run locally (make run, make
#     run-controller) with `automatic-upgrades: true` in
#     hack/controller.local.yaml and `aws-management-role-arn` in
#     hack/config.local.yaml;
#   - AWS credentials of the demo account are exported in this shell
#     (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, or AWS_PROFILE); they are
#     never typed on screen;
#   - DEMO_ACCOUNT_ID is the 12-digit ID of that account.
#
# Every run has its own names, from DEMO_RUN (default: the time, MMDDHHMM):
# tenant demo-<run>, account demo-<run>, network demo-<run>-net, cluster
# demo-<run>-eks, role and stack cluster-vending-demo-<run>-provisioner. A
# run never touches another run's resources, or anything else in the
# account; another run can start while one is still deleting.
#
# It runs for real and takes about 90 minutes (EKS create, upgrade and
# delete); the waits are hidden, so the recording lasts a few minutes. A
# step that fails stops the recording; `make demo` prints why
# (bin/demo-failed.txt).

Output bin/demo.gif
Output bin/demo.mp4

Require cluster-vending
Require jq
Require aws

Set Shell bash
Set FontSize 15
Set Width 1500
Set Height 900
Set Padding 20
Set TypingSpeed 35ms
Set Theme "Catppuccin Mocha"
Set WindowBar Colorful

# ---------------------------------------------------------------- setup
Hide
Type `export PS1='\[\e[1;36m\]❯ \[\e[0m\]' PATH="$PWD/bin:$PATH"` Enter
Type `: "${DEMO_ACCOUNT_ID:?export DEMO_ACCOUNT_ID first}"; export RUN="${DEMO_RUN:-$(date +%m%d%H%M)}"` Enter
Type `export T="${DEMO_TENANT:-demo-$RUN}" ROLE="cluster-vending-demo-$RUN-provisioner" DEMO_REGION="${DEMO_REGION:-us-west-1}" DEMO_ZONE_A="${DEMO_ZONE_A:-usw1-az1}" DEMO_ZONE_B="${DEMO_ZONE_B:-usw1-az3}" D="$(mktemp -d)"` Enter
Type `source hack/demo/lib.sh; fill() { sed -e "s/__RUN__/$RUN/g" -e "s/__TENANT__/$T/g" -e "s/__ACCOUNT_ID__/$DEMO_ACCOUNT_ID/g" -e "s/__BOOTSTRAP_ID__/$ACCOUNT/g" -e "s/__NETWORK_ID__/$NETWORK/g" -e "s/__REGION__/$DEMO_REGION/g" -e "s/__ZONE_A__/$DEMO_ZONE_A/g" -e "s/__ZONE_B__/$DEMO_ZONE_B/g" "hack/demo/$1" > "$D/$1"; }` Enter
Type `fill cloudaccount.yaml; clear` Enter
Show

# ---------------------------------------------------------- 1. account
Type "# 1. Onboard the AWS account: the CLI creates the platform's role with CloudFormation, then the controller checks access" Enter
Sleep 1s
Type `cat $D/cloudaccount.yaml | grep -v '^#'` Enter
Sleep 3s
Type `cluster-vending account create -f $D/cloudaccount.yaml --create-role terminal --region $DEMO_REGION` Enter
Sleep 5s
Hide
Wait@40m /^❯\s*$/
Type `wait_account 1 $?` Enter
Wait+Screen@30m /__step_1_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "clear" Enter
Show
Type `cluster-vending account list --tenant $T` Enter
Sleep 5s

# ---------------------------------------------------------- 2. network
Hide
Type `fill network.yaml; clear` Enter
Show
Type "# 2. A network: VPC, two zones, public and private subnets, one NAT gateway" Enter
Sleep 1s
Type `cluster-vending network create -f $D/network.yaml` Enter
Sleep 6s
Hide
Wait@10m /^❯\s*$/
Type `wait_network 2 $?` Enter
Wait+Screen@45m /__step_2_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "clear" Enter
Show
Type `cluster-vending network list --tenant $T` Enter
Sleep 4s

# ---------------------------------------------------------- 3. cluster
Hide
Type `fill cluster.yaml; clear` Enter
Show
Type "# 3. An EKS cluster on Kubernetes 1.36, on the alpha channel (1.37), upgraded automatically, tier 2" Enter
Sleep 1s
Type `grep -E 'kubernetesVersion|releaseChannel|upgradeMode|tier' $D/cluster.yaml` Enter
Sleep 3s
Type `cluster-vending cluster create -f $D/cluster.yaml` Enter
Sleep 6s
Hide
Wait@10m /^❯\s*$/
Type `wait_cluster 3 $?` Enter
Wait+Screen@80m /__step_3_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "clear" Enter
Show
Type `cluster-vending cluster get --id $CLUSTER --tenant $T | head -22` Enter
Sleep 7s

# ---------------------------------------------------------- 4. upgrade
Type "clear" Enter
Type "# 4. The alpha channel's rollout picks the cluster and upgrades it to 1.37: control plane, then node groups and add-ons" Enter
Sleep 1s
Type "cluster-vending admin rollout get --channel alpha" Enter
Sleep 5s
Hide
Wait@10m /^❯\s*$/
Type `wait_upgrade_started 4` Enter
Wait+Screen@35m /__step_4_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "clear" Enter
Show
Type `cluster-vending cluster get --id $CLUSTER --tenant $T | grep -E '^(Version|Channel|Tier|Upgrades|Rollout|Upgrade):'` Enter
Sleep 5s
Type `cluster-vending cluster events --id $CLUSTER --tenant $T | head -8` Enter
Sleep 6s
Hide
Wait@10m /^❯\s*$/
Type `wait_upgraded 5` Enter
Wait+Screen@125m /__step_5_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "clear" Enter
Show
Type "# Upgraded:" Enter
Type `cluster-vending cluster get --id $CLUSTER --tenant $T | grep -E '^(Version|Rollout|Phase):'` Enter
Sleep 4s
Type `cluster-vending cluster events --id $CLUSTER --tenant $T | head -6` Enter
Sleep 6s

# ---------------------------------------------------------- 5. delete
Type "clear" Enter
Type "# 5. Delete everything: the cluster, the network, then the account and its role's stack" Enter
Sleep 1s
Type `cluster-vending cluster delete --id $CLUSTER --tenant $T` Enter
Sleep 6s
Hide
Wait@10m /^❯\s*$/
Type `wait_gone 6 $? cluster "$CLUSTER" 3600` Enter
Wait+Screen@65m /__step_6_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "clear" Enter
Show
Type `cluster-vending network delete --id $NETWORK --tenant $T` Enter
Sleep 6s
Hide
Wait@10m /^❯\s*$/
Type `wait_gone 7 $? network "$NETWORK" 2400` Enter
Wait+Screen@45m /__step_7_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "clear" Enter
Show
Type `cluster-vending account delete --id $ACCOUNT --tenant $T --delete-role --region $DEMO_REGION --wait` Enter
Sleep 8s
Hide
Wait@10m /^❯\s*$/
Type `wait_account_gone 8 $?` Enter
Wait+Screen@35m /__step_8_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "clear" Enter
Show
Type `cluster-vending account list --tenant $T; cluster-vending network list --tenant $T; cluster-vending cluster list --tenant $T` Enter
Sleep 5s
Hide
Type `rm -rf "$D"` Enter
