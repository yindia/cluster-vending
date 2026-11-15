# Demo phase 5 of 5: delete everything the run made: the cluster, the
# network, then the account and its role's stack. Pieces gone already (an
# earlier attempt, or a phase that never ran) are skipped. Marks the run
# finished. `make demo-cleanup`.

Output bin/demo/05-cleanup.gif
Output bin/demo/05-cleanup.mp4

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

Source hack/demo/setup.tape

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
Type `cluster-vending account delete --id $ACCOUNT --tenant $T --wait && aws cloudformation delete-stack --stack-name $ROLE --region $DEMO_REGION` Enter
Sleep 8s
Hide
Wait@10m /^❯\s*$/
Type `wait_account_gone 8 $?` Enter
Wait+Screen@35m /__step_8_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "demo_finish; clear" Enter
Show
Type `cluster-vending account list --tenant $T; cluster-vending network list --tenant $T; cluster-vending cluster list --tenant $T` Enter
Sleep 5s
Hide
Type `rm -rf "$D"` Enter
