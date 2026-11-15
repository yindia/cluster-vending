# Demo phase 3 of 5: an EKS cluster in the run's network, on Kubernetes
# 1.36, on the alpha channel (1.37), upgraded automatically, tier 2.
# `make demo-cluster`.

Output bin/demo/03-cluster.gif
Output bin/demo/03-cluster.mp4

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

Hide
Type "fill cluster.yaml; clear" Enter
Show
Type "# 3. An EKS cluster on Kubernetes 1.36, on the alpha channel (1.37), upgraded automatically, tier 2" Enter
Sleep 1s
Type `grep -E 'kubernetesVersion|releaseChannel|upgradeMode|tier|irsa' $D/cluster.yaml` Enter
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
