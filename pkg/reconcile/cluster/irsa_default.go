package cluster

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// withIRSA defaults a new cluster's spec.aws.irsa: the platform components
// installed with every cluster (the load balancer controller, external
// secrets, Karpenter, ...) reach AWS through IRSA roles, which need the
// cluster's IAM OIDC provider. Only creates: an imported or taken-over
// cluster keeps the OIDC setup it came with.
func (s *Reconciler) withIRSA(ctx context.Context, spec *cloudv1.ClusterSpec) error {
	if spec.GetAws() == nil {
		return nil
	}
	var users []string
	if spec.GetAws().Irsa != nil && !spec.GetAws().GetIrsa() && s.deps.Platform != nil {
		defs, err := s.deps.Platform.Definitions(ctx)
		if err != nil {
			return fault.New(fault.Unavailable, "cannot read the component catalog; try again")
		}
		entries, err := component.PlatformEntries(defs)
		if err != nil {
			return fault.New(fault.FailedPrecondition, "the component catalog is inconsistent: "+err.Error())
		}
		users = irsaUsers(entries, "aws")
	}
	return defaultIRSA(spec, users)
}

// defaultIRSA turns IRSA on when the spec leaves it unset, and refuses it
// off while users (platform components with IRSA roles) need it.
func defaultIRSA(spec *cloudv1.ClusterSpec, users []string) error {
	aws := spec.GetAws()
	switch {
	case aws == nil:
		return nil
	case aws.Irsa == nil:
		aws.Irsa = proto.Bool(true)
	case !aws.GetIrsa() && len(users) > 0:
		return fault.New(fault.Invalid, fmt.Sprintf(
			"spec.aws.irsa is false, but platform components installed with every cluster use IRSA: %s; leave it unset or set it to true",
			strings.Join(users, ", ")))
	}
	return nil
}

// irsaUsers are the platform components for provider whose IAM role is
// reached through IRSA.
func irsaUsers(entries []component.PlatformEntry, provider string) []string {
	var out []string
	for _, e := range entries {
		if e.Disabled || !e.Covers(provider) {
			continue
		}
		if r := e.CloudResources.IAMRole; r != nil && r.UsesIRSA() {
			out = append(out, e.Name)
		}
	}
	return out
}
