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

// reservedTagPrefixes are the tag key prefixes the platform writes itself
// (ownership, Karpenter and Kubernetes discovery): a label with such a key
// would be overridden by the platform's tag on every resource, silently.
// k8s.io/cluster-autoscaler/ stays reserved though the platform no longer
// writes it: labels become the system node group's tags, which EKS copies
// to its Auto Scaling group, and those tags would make the static group
// one a cluster-autoscaler discovers (one still running on a cluster that
// has not removed it yet, or one a tenant brings) and resizes. Matched
// without regard to case.
var reservedTagPrefixes = []string{"cluster-vending.io/", "karpenter.sh/", "kubernetes.io/cluster/", "k8s.io/cluster-autoscaler/"}

// CheckAWSLabels refuses labels AWS cannot take as tags: keys starting
// with "aws:" are reserved, and so are the keys the platform tags its
// resources with (reservedTagPrefixes; not "Name": on a taken-over network a
// Name label replaces the imported tag on purpose, see
// TestALabelThatGoesGivesTheImportedTagBack). The other tag rules
// (characters, lengths, 40 pairs) are in the contract. Labels become the AWS
// tags of every resource of the network or cluster.
func CheckAWSLabels(labels map[string]string) error {
	for k := range labels {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "aws:") {
			return fault.New(fault.Invalid, fmt.Sprintf("label %q: AWS tag keys must not start with %q", k, "aws:"))
		}
		for _, prefix := range reservedTagPrefixes {
			if strings.HasPrefix(lower, prefix) {
				return fault.New(fault.Invalid, fmt.Sprintf("label %q: AWS tag keys starting with %q are set by the platform", k, prefix))
			}
		}
	}
	return nil
}
