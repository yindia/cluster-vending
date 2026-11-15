# Demo phase 1 of 5: onboard the AWS account. account create onboards it
# and prints how to create the platform's role
# (cluster-vending-demo-<run>-provisioner) with CloudFormation; the tape
# runs that command, then account verify --wait has the controller check
# access. Run it with
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
Type "# 1. Onboard the AWS account: account create prints how to create the platform's role with CloudFormation" Enter
Sleep 1s
Type `cat $D/cloudaccount.yaml | grep -v '^#'` Enter
Sleep 3s
Type `cluster-vending account create -f $D/cloudaccount.yaml | tee $D/account.out` Enter
Wait@5m /^❯\s*$/
Sleep 6s
Type "# 2. Create the role: the command account create printed, in the demo's region" Enter
Type `sed -n '/^  curl -fsSL/,/--region/p' $D/account.out | sed "s/--region us-east-1/--region $DEMO_REGION/" | tee $D/role.sh` Enter
Sleep 4s
Type `bash $D/role.sh && aws cloudformation wait stack-create-complete --stack-name $ROLE --region $DEMO_REGION` Enter
Wait@30m /^❯\s*$/
Hide
Type `found_account; clear` Enter
Show
Type "# 3. The stack is there: the controller checks the account's access" Enter
Type `cluster-vending account verify --id $ACCOUNT --tenant $T --region $DEMO_REGION --wait` Enter
Wait@30m /^❯\s*$/
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
