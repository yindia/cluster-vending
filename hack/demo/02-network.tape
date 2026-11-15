# Demo phase 2 of 5: a network in the run's account: a VPC, two zones,
# public and private subnets, one NAT gateway. `make demo-network`.

Output bin/demo/02-network.gif
Output bin/demo/02-network.mp4

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
Type "fill network.yaml; clear" Enter
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
