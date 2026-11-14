# cluster-vending demo: onboard an AWS account, create a network and an EKS
# cluster on Kubernetes 1.36, watch the alpha channel upgrade it to 1.37 by
# itself, then delete everything the demo created.
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
# It runs for real and takes about 90 minutes (EKS create, upgrade and
# delete); the waits are hidden, so the recording lasts a few minutes.
# It uses its own tenant (demo) and names (demo-net, demo-eks): nothing
# else is touched. The role's CloudFormation stack is created and deleted
# only if the account has no cluster-vending-provisioner role yet.

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
Type `export PS1='\[\e[1;36m\]$ \[\e[0m\]' PATH="$PWD/bin:$PATH"` Enter
Type `: "${DEMO_ACCOUNT_ID:?export DEMO_ACCOUNT_ID first}"; export DEMO_REGION="${DEMO_REGION:-us-west-1}" DEMO_ZONE_A="${DEMO_ZONE_A:-usw1-az1}" DEMO_ZONE_B="${DEMO_ZONE_B:-usw1-az3}"` Enter
Type `if aws iam get-role --role-name cluster-vending-provisioner >/dev/null 2>&1; then export DEMO_CREATE_ROLE=skip; else export DEMO_CREATE_ROLE=terminal; fi` Enter
Type `export D="$(mktemp -d)"; sed "s/__ACCOUNT_ID__/$DEMO_ACCOUNT_ID/" hack/demo/cloudaccount.yaml > "$D/cloudaccount.yaml"` Enter
Type `step() { echo "__step_$1_done__"; }` Enter
Type `json() { cluster-vending "$@" --tenant demo -o json; }` Enter
Type "clear" Enter
Show

# ---------------------------------------------------------- 1. account
Type "# 1. Onboard the AWS account: the CLI creates the platform's role with CloudFormation, then checks access" Enter
Sleep 1s
Type `cluster-vending account create -f $D/cloudaccount.yaml --create-role $DEMO_CREATE_ROLE --region $DEMO_REGION` Enter
Sleep 6s
Hide
Type `export ACCOUNT="$(json account list | jq -r '.accounts[] | select(.name=="demo") | .id')"; until json account get --id "$ACCOUNT" | jq -e '.account.status.access.verified' >/dev/null; do sleep 10; done; step 1` Enter
Wait+Screen@20m /__step_1_done__/
Type "clear" Enter
Show
Type `cluster-vending account get --id $ACCOUNT --tenant demo` Enter
Sleep 5s

# ---------------------------------------------------------- 2. network
Hide
Type `sed -e "s/__BOOTSTRAP_ID__/$ACCOUNT/" -e "s/__REGION__/$DEMO_REGION/" -e "s/__ZONE_A__/$DEMO_ZONE_A/g" -e "s/__ZONE_B__/$DEMO_ZONE_B/g" hack/demo/network.yaml > "$D/network.yaml"; clear` Enter
Show
Type "# 2. A network: VPC, two zones, public and private subnets, one NAT gateway" Enter
Sleep 1s
Type `cluster-vending network create -f $D/network.yaml --wait` Enter
Sleep 8s
Hide
Type "step 2" Enter
Wait+Screen@40m /__step_2_done__/
Type `export NETWORK="$(json network list | jq -r '.networks[] | select(.name=="demo-net") | .id')"; clear` Enter
Show
Type `cluster-vending network list --tenant demo` Enter
Sleep 4s

# ---------------------------------------------------------- 3. cluster
Hide
Type `sed -e "s/__NETWORK_ID__/$NETWORK/" -e "s/__REGION__/$DEMO_REGION/" hack/demo/cluster.yaml > "$D/cluster.yaml"; clear` Enter
Show
Type "# 3. An EKS cluster on Kubernetes 1.36, on the alpha channel (1.37), upgraded automatically, tier 2" Enter
Sleep 1s
Type `grep -E 'kubernetesVersion|releaseChannel|upgradeMode|tier' $D/cluster.yaml` Enter
Sleep 3s
Type `cluster-vending cluster create -f $D/cluster.yaml --wait` Enter
Sleep 8s
Hide
Type "step 3" Enter
Wait+Screen@75m /__step_3_done__/
Type `export CLUSTER="$(json cluster list | jq -r '.clusters[] | select(.name=="demo-eks") | .id')"; clear` Enter
Show
Type `cluster-vending cluster get --id $CLUSTER --tenant demo | head -22` Enter
Sleep 7s

# ---------------------------------------------------------- 4. upgrade
Type "clear" Enter
Type "# 4. The alpha channel's rollout picks the cluster and upgrades it to 1.37: control plane, then node groups and add-ons" Enter
Sleep 1s
Type "cluster-vending admin rollout get --channel alpha" Enter
Sleep 5s
Hide
Type `until [ -n "$(json cluster get --id "$CLUSTER" | jq -r '.cluster.status.upgrade.phase // empty')" ]; do sleep 15; done; step 4; clear` Enter
Wait+Screen@30m /__step_4_done__/
Show
Type `cluster-vending cluster get --id $CLUSTER --tenant demo | grep -E '^(Version|Channel|Tier|Upgrades|Rollout|Upgrade):'` Enter
Sleep 5s
Type `cluster-vending cluster events --id $CLUSTER --tenant demo | head -8` Enter
Sleep 6s
Hide
Type `until json cluster get --id "$CLUSTER" | jq -e '(.cluster.status.currentKubernetesVersion | startswith("1.37")) and (.cluster.status.upgrade == null)' >/dev/null; do sleep 30; done; step 5; clear` Enter
Wait+Screen@120m /__step_5_done__/
Show
Type "# Upgraded:" Enter
Type `cluster-vending cluster get --id $CLUSTER --tenant demo | grep -E '^(Version|Rollout|Phase):'` Enter
Sleep 4s
Type `cluster-vending cluster events --id $CLUSTER --tenant demo | head -6` Enter
Sleep 6s

# ---------------------------------------------------------- 5. delete
Type "clear" Enter
Type "# 5. Delete everything the demo created: the cluster, the network, and the role's stack if the demo created it" Enter
Sleep 1s
Type `cluster-vending cluster delete --id $CLUSTER --tenant demo --wait` Enter
Sleep 6s
Hide
Type "step 6" Enter
Wait+Screen@60m /__step_6_done__/
Type "clear" Enter
Show
Type `cluster-vending network delete --id $NETWORK --tenant demo --wait` Enter
Sleep 6s
Hide
Type "step 7" Enter
Wait+Screen@40m /__step_7_done__/
Type "clear" Enter
Show
Type `[ "$DEMO_CREATE_ROLE" = terminal ] && aws cloudformation delete-stack --stack-name cluster-vending-provisioner --region $DEMO_REGION && aws cloudformation wait stack-delete-complete --stack-name cluster-vending-provisioner --region $DEMO_REGION; echo "stack: done"` Enter
Hide
Wait+Screen@20m /^stack: done/
Show
Type `cluster-vending cluster list --tenant demo; cluster-vending network list --tenant demo` Enter
Sleep 5s
Hide
Type `rm -rf "$D"` Enter
