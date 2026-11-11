package service

import (
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"
)

// CheckAWSLabels refuses labels AWS cannot take as tags: keys starting
// with "aws:" are reserved. The other tag rules (characters, lengths, 40
// pairs) are in the contract. Labels become the AWS tags of every
// resource of the network or cluster.
func CheckAWSLabels(labels map[string]string) error {
	for k := range labels {
		if strings.HasPrefix(strings.ToLower(k), "aws:") {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Sprintf("label %q: AWS tag keys must not start with %q", k, "aws:"))
		}
	}
	return nil
}
