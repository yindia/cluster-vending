# cluster-vending demo: with an onboarded, verified AWS account, create a
# network and an EKS cluster on Kubernetes 1.36, watch the alpha channel
# upgrade it to 1.37 by itself, then delete everything the demo created.
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
#   - DEMO_TENANT (default acme) has a verified account (account list);
#     the demo creates no account and no role.
#
# It runs for real and takes about 90 minutes (EKS create, upgrade and
# delete); the waits are hidden, so the recording lasts a few minutes.
# It uses its own names (demo-net, demo-eks): nothing else in the tenant is
# touched.

Output bin/demo.gif
Output bin/demo.mp4

Require cluster-vending
Require jq

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
Type `export T="${DEMO_TENANT:-acme}" DEMO_REGION="${DEMO_REGION:-us-west-1}" DEMO_ZONE_A="${DEMO_ZONE_A:-usw1-az1}" DEMO_ZONE_B="${DEMO_ZONE_B:-usw1-az3}" D="$(mktemp -d)"` Enter
Type `step() { echo "__step_$1_done__"; }` Enter
Type `json() { cluster-vending "$@" --tenant "$T" -o json; }` Enter
Type `export ACCOUNT="$(json account list | jq -r '[.accounts[] | select(.status.access.verified)][0].id // empty')"; [ -n "$ACCOUNT" ] || echo "no verified account in tenant $T"` Enter
Type "clear" Enter
Show

# ---------------------------------------------------------- 1. account
Type "# 1. The tenant's AWS account: onboarded, and its access verified by the controller" Enter
Sleep 1s
Type `cluster-vending account list --tenant $T` Enter
Sleep 5s

# ---------------------------------------------------------- 2. network
Hide
Type `sed -e "s/__TENANT__/$T/" -e "s/__BOOTSTRAP_ID__/$ACCOUNT/" -e "s/__REGION__/$DEMO_REGION/" -e "s/__ZONE_A__/$DEMO_ZONE_A/g" -e "s/__ZONE_B__/$DEMO_ZONE_B/g" hack/demo/network.yaml > "$D/network.yaml"; clear` Enter
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
Type `cluster-vending network list --tenant $T` Enter
Sleep 4s

# ---------------------------------------------------------- 3. cluster
Hide
Type `sed -e "s/__TENANT__/$T/" -e "s/__NETWORK_ID__/$NETWORK/" -e "s/__REGION__/$DEMO_REGION/" hack/demo/cluster.yaml > "$D/cluster.yaml"; clear` Enter
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
Type `cluster-vending cluster get --id $CLUSTER --tenant $T | head -22` Enter
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
Type `cluster-vending cluster get --id $CLUSTER --tenant $T | grep -E '^(Version|Channel|Tier|Upgrades|Rollout|Upgrade):'` Enter
Sleep 5s
Type `cluster-vending cluster events --id $CLUSTER --tenant $T | head -8` Enter
Sleep 6s
Hide
Type `until json cluster get --id "$CLUSTER" | jq -e '(.cluster.status.currentKubernetesVersion | startswith("1.37")) and (.cluster.status.upgrade == null)' >/dev/null; do sleep 30; done; step 5; clear` Enter
Wait+Screen@120m /__step_5_done__/
Show
Type "# Upgraded:" Enter
Type `cluster-vending cluster get --id $CLUSTER --tenant $T | grep -E '^(Version|Rollout|Phase):'` Enter
Sleep 4s
Type `cluster-vending cluster events --id $CLUSTER --tenant $T | head -6` Enter
Sleep 6s

# ---------------------------------------------------------- 5. delete
Type "clear" Enter
Type "# 5. Delete everything the demo created: the cluster, then the network" Enter
Sleep 1s
Type `cluster-vending cluster delete --id $CLUSTER --tenant $T --wait` Enter
Sleep 6s
Hide
Type "step 6" Enter
Wait+Screen@60m /__step_6_done__/
Type "clear" Enter
Show
Type `cluster-vending network delete --id $NETWORK --tenant $T --wait` Enter
Sleep 6s
Hide
Type "step 7" Enter
Wait+Screen@40m /__step_7_done__/
Type "clear" Enter
Show
Type `cluster-vending cluster list --tenant $T; cluster-vending network list --tenant $T` Enter
Sleep 5s
Hide
Type `rm -rf "$D"` Enter
