package reconcile

import (
	"fmt"
	"strings"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/fault"
)

// Validate checks a message against its protovalidate rules (the
// buf.validate options in idl/); a violation is an Invalid fault.
func Validate(msg proto.Message) error {
	if err := protovalidate.Validate(msg); err != nil {
		return fault.New(fault.Invalid, err.Error())
	}
	return nil
}

// CheckAWSLabels refuses labels AWS cannot take as tags: keys starting
// with "aws:" are reserved. The other tag rules (characters, lengths, 40
// pairs) are in the contract. Labels become the AWS tags of every
// resource of the network or cluster.
func CheckAWSLabels(labels map[string]string) error {
	for k := range labels {
		if strings.HasPrefix(strings.ToLower(k), "aws:") {
			return fault.New(fault.Invalid, fmt.Sprintf("label %q: AWS tag keys must not start with %q", k, "aws:"))
		}
	}
	return nil
}
