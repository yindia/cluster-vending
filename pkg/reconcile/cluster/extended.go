package cluster

import (
	"context"
	"fmt"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/release"
)

// A Kubernetes minor the cluster's release offers on no channel is an
// extended-support minor (EKS extended support): it is still supported,
// but no channel leads to it, so no rollout ever moves a cluster onto it
// as its channel's minor. A cluster on one runs with upgradeMode Manual
// (unset is Manual) and on EKS extended support: an unset supportType is
// rendered EXTENDED, and STANDARD is refused, since AWS upgrades the
// control plane by force when standard support ends.
//
// The rule is checked on API writes (NewRow, UpdateCluster) and on edits
// of the Cluster object; never in a reconcile pass. An automatic cluster
// left on a minor a release no longer puts on a channel is upgraded by
// its channel's rollout as before, and a write that does not move it into
// the rule's breach (an unrelated field) is not refused.

// extendedOffer is the offer of the release the spec takes its platform
// components from (its channel's, else stable's) and whether the spec's
// Kubernetes minor is on no channel there. nil and false, with no error,
// when the reconciler reads no releases, the spec names no version, or the
// channel offers no release yet.
func (s *Reconciler) extendedOffer(ctx context.Context, spec *cloudv1.ClusterSpec) (*channelOffer, bool, error) {
	version := spec.GetControlPlane().GetKubernetesVersion()
	src := s.releases()
	if version == "" || src == nil {
		return nil, false, nil
	}
	o, err := readOffer(ctx, src, channelOf(spec), s.logger())
	if err != nil || o == nil {
		return nil, false, err
	}
	return o, release.Ending(o.rel, minorString(version)), nil
}

// CheckExtendedSupport refuses a spec on an extended-support minor that is
// automatic or names supportType STANDARD (spec is the new one; before is
// the cluster's spec before the write, nil for a create). A write that
// leaves a cluster in the breach it was already in (an automatic cluster
// whose minor lost its channel, changing another field) passes: such a
// cluster is upgraded by its rollout. A release that cannot be read is
// UNAVAILABLE.
func (s *Reconciler) CheckExtendedSupport(ctx context.Context, spec, before *cloudv1.ClusterSpec) error {
	refusal, err := s.extendedRefusal(ctx, spec, before)
	if refusal == nil || err != nil {
		return err
	}
	if before != nil {
		was, err := s.extendedRefusal(ctx, before, nil)
		if err != nil {
			return err
		}
		if was != nil && was.rule == refusal.rule {
			return nil
		}
	}
	return refusal.fault
}

// CheckExtendedEdit is CheckExtendedSupport for an edit of the Cluster
// object, which the store has already taken: the edited spec is the
// row's, and what it was before is the stored input (what the API last
// accepted) with, where the input names none, the version the cluster runs
// and the edited spec's.
func (s *Reconciler) CheckExtendedEdit(ctx context.Context, row *model.Cluster) error {
	spec, err := SpecOf(row)
	if err != nil {
		return err
	}
	before := &cloudv1.ClusterSpec{}
	if len(row.Input) > 0 {
		input := &cloudv1.Cluster{}
		if err := Lenient.Unmarshal(row.Input, input); err != nil {
			return fmt.Errorf("cluster %s: stored input: %w", row.ID, err)
		}
		if input.GetSpec() != nil {
			before = input.GetSpec()
		}
	}
	if before.GetControlPlane().GetKubernetesVersion() == "" {
		version := storedStatus(row).GetCurrentKubernetesVersion()
		if version == "" {
			version = spec.GetControlPlane().GetKubernetesVersion()
		}
		if before.ControlPlane == nil {
			before.ControlPlane = &cloudv1.ControlPlaneSpec{}
		}
		before.ControlPlane.KubernetesVersion = version
	}
	corecluster.ResolveReleaseFields(before)
	return s.CheckExtendedSupport(ctx, spec, before)
}

// extendedRule is which part of the rule a spec breaks.
type extendedRule int

const (
	ruleAutomatic extendedRule = iota + 1
	ruleStandard
)

type extendedBreach struct {
	rule  extendedRule
	fault error
}

// extendedRefusal is the breach of spec, nil for none. before (nil: none
// known) only chooses the wording: a cluster that stays on its minor and
// turns automatic is told to upgrade first.
func (s *Reconciler) extendedRefusal(ctx context.Context, spec, before *cloudv1.ClusterSpec) (*extendedBreach, error) {
	o, extended, err := s.extendedOffer(ctx, spec)
	if err != nil || !extended {
		return nil, err
	}
	minor := minorString(spec.GetControlPlane().GetKubernetesVersion())
	head := fmt.Sprintf("Kubernetes %s is an extended-support version (the release offers it on no channel)", minor)
	if spec.GetUpgradeMode() == cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC {
		stays := before != nil && minorString(before.GetControlPlane().GetKubernetesVersion()) == minor
		if stays {
			return &extendedBreach{rule: ruleAutomatic, fault: fault.New(fault.FailedPrecondition, fmt.Sprintf(
				"%s: it runs with upgradeMode Manual only; upgrade to %s first (or keep Manual)", head, upgradeToOf(o, minor)))}, nil
		}
		return &extendedBreach{rule: ruleAutomatic, fault: fault.New(fault.Invalid, fmt.Sprintf(
			"%s: it runs with upgradeMode Manual only; set upgradeMode UPGRADE_MODE_MANUAL, or choose %s",
			head, oldestChannelMinor(o)))}, nil
	}
	if spec.GetAws().GetUpgradePolicy().GetSupportType() == cloudv1.AwsSupportType_AWS_SUPPORT_TYPE_STANDARD {
		return &extendedBreach{rule: ruleStandard, fault: fault.New(fault.Invalid, fmt.Sprintf(
			"%s: spec.aws.upgradePolicy.supportType AWS_SUPPORT_TYPE_STANDARD would have AWS upgrade the control plane by force "+
				"when standard support for %s ends; leave supportType unset (EXTENDED on this minor) or set AWS_SUPPORT_TYPE_EXTENDED", head, minor))}, nil
	}
	return nil, nil
}

// upgradeToOf is the minor after minor: the release's upgradeTo for it,
// else the next minor it offers.
func upgradeToOf(o *channelOffer, minor string) string {
	for _, k := range o.rel.Kubernetes {
		if k.Minor == minor && k.UpgradeTo != "" {
			return k.UpgradeTo
		}
	}
	return nextOffered(o, minor)
}

// oldestChannelMinor is "1.35 or later" for a release whose oldest minor
// on a channel is 1.35; "a minor on a release channel" when it puts none.
func oldestChannelMinor(o *channelOffer) string {
	oldest := ""
	for _, k := range o.rel.Kubernetes {
		if k.Channel != "" && (oldest == "" || newer(oldest, k.Minor)) {
			oldest = k.Minor
		}
	}
	if oldest == "" {
		return "a minor on a release channel"
	}
	return oldest + " or later"
}

// supportTypeExtended is the provider's spelling of EKS extended support.
var supportTypeExtended = corecluster.CrossplaneName(cloudv1.AwsSupportType_AWS_SUPPORT_TYPE_EXTENDED, "EXTENDED")

// ForTakeover is a refusal of CheckExtendedSupport as a takeover reports
// it: an imported cluster's spec is what EKS reports, so the message adds
// where to change it. Other errors are returned as they are.
func ForTakeover(err error) error {
	f, ok := fault.As(err)
	if !ok || (f.Kind != fault.Invalid && f.Kind != fault.FailedPrecondition) {
		return err
	}
	return fault.New(f.Kind, f.Message+
		"; the imported cluster's settings are the ones EKS reports: change them in EKS (or upgrade the cluster there), then import it again to take it over")
}
