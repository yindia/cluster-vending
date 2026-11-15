# Demo phase 4 of 5: the alpha channel's rollout picks the run's cluster
# and upgrades it to 1.37 by itself: control plane, then node groups and
# add-ons. Needs the controller's automatic-upgrades on. `make demo-upgrade`.

Output bin/demo/04-upgrade.gif
Output bin/demo/04-upgrade.mp4

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

Source hack/demo/setup.tape

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
