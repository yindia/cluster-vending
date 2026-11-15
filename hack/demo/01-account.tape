# Demo phase 1 of 5: onboard the AWS account. The CLI creates the
# platform's role (cluster-vending-demo-<run>-provisioner) with
# CloudFormation, then the controller checks access. Run it with
# `make demo-account` (hack/demo/run.sh: the run, the outputs, a resume).

Output bin/demo/01-account.gif
Output bin/demo/01-account.mp4

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

Hide
Type "fill cloudaccount.yaml; clear" Enter
Show
Type "# 1. Onboard the AWS account: the CLI creates the platform's role with CloudFormation, then the controller checks access" Enter
Sleep 1s
Type `cat $D/cloudaccount.yaml | grep -v '^#'` Enter
Sleep 3s
Type `cluster-vending account create -f $D/cloudaccount.yaml --create-role terminal --region $DEMO_REGION` Enter
# Shown until it returns: the stack's events, then the access check.
Wait@40m /^❯\s*$/
Sleep 4s
Hide
Type `wait_account 1 $?` Enter
Wait+Screen@30m /__step_1_(done|failed)__/
Type "check" Enter
Wait+Screen@10s /__continue__/
Type "clear" Enter
Show
Type `cluster-vending account list --tenant $T` Enter
Sleep 5s
