package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/release"
	kubeupgrade "github.com/yindia/cluster-vending/pkg/upgrade"
)

// channelOffer is what a release channel offers of Kubernetes now: the
// release it offers and the minor that release puts on the channel.
type channelOffer struct {
	channel string
	// minor is the channel's minor; "" when the release puts none on it
	// (a release published before channels were assigned per minor).
	minor string
	rel   release.Release
}

// offered are the minors of the channel's release, newest first as the
// release lists them.
func (o *channelOffer) offered() []string {
	out := make([]string, 0, len(o.rel.Kubernetes))
	for _, k := range o.rel.Kubernetes {
		out = append(out, k.Minor)
	}
	return out
}

// releases is where the cluster's channel and release are read from:
// Releases, else Platform; nil when the reconciler has neither (tests).
func (s *Reconciler) releases() reconcile.ReleaseReader {
	if s.deps.Releases != nil {
		return s.deps.Releases
	}
	if s.deps.Platform != nil {
		return s.deps.Platform
	}
	return nil
}

// namedChannel is the release channel the spec names (alpha, beta,
// stable); false when it names none.
func namedChannel(spec *cloudv1.ClusterSpec) (string, bool) {
	if spec.GetReleaseChannel() == cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED {
		return "", false
	}
	return channelOf(spec), true
}

// readOffer reads what channel offers. Nil, with no error, when the
// channel offers no release yet or its release is gone: there is nothing
// to check a version against. A read that fails is UNAVAILABLE, never
// skipped: a version would otherwise go unchecked.
func readOffer(ctx context.Context, src reconcile.ReleaseReader, channel string, log *slog.Logger) (*channelOffer, error) {
	unreadable := func(what string, err error) error {
		// The management cluster's error is the log's: it may name its
		// internals.
		log.ErrorContext(ctx, "read "+what, "error", err)
		return fault.New(fault.Unavailable, fmt.Sprintf("cannot read %s from the management cluster; try again", what))
	}
	name, err := src.Offered(ctx, channel)
	if err != nil {
		return nil, unreadable("release channel "+channel, err)
	}
	if name == "" {
		return nil, nil
	}
	rel, err := src.Release(ctx, name)
	if err != nil {
		return nil, unreadable(fmt.Sprintf("release %s of channel %s", name, channel), err)
	}
	if rel == nil {
		return nil, nil
	}
	r := reconcile.ReleaseOf(rel.Name, rel.Spec.Content)
	minor, _ := release.ChannelMinor(r, channel)
	return &channelOffer{channel: channel, minor: minor, rel: r}, nil
}

// withChannelVersion checks spec's Kubernetes version against the channel
// it names, and gives a spec on a channel that names no version the
// channel's minor. A spec on no channel is left as it is: its version is
// the client's, or EKS's default.
func (s *Reconciler) withChannelVersion(ctx context.Context, spec *cloudv1.ClusterSpec) error {
	channel, ok := namedChannel(spec)
	src := s.releases()
	if src == nil {
		return nil
	}
	if !ok {
		return s.withReleaseVersion(ctx, src, spec)
	}
	o, err := readOffer(ctx, src, channel, s.logger())
	if err != nil || o == nil || o.minor == "" {
		return err
	}
	if spec.GetControlPlane().GetKubernetesVersion() == "" {
		if spec.ControlPlane == nil {
			spec.ControlPlane = &cloudv1.ControlPlaneSpec{}
		}
		spec.ControlPlane.KubernetesVersion = o.minor
		return nil
	}
	return checkChannelVersion(o, spec.GetControlPlane().GetKubernetesVersion())
}

// withReleaseVersion is withChannelVersion for a spec on no channel: its
// platform components come from the stable channel's release, so a version
// that release does not offer is refused (FAILED_PRECONDITION, naming the
// offered minors), and a spec that names none gets the release's default
// minor (else the stable channel's), not EKS's default.
func (s *Reconciler) withReleaseVersion(ctx context.Context, src reconcile.ReleaseReader, spec *cloudv1.ClusterSpec) error {
	o, err := readOffer(ctx, src, channelOf(spec), s.logger())
	if err != nil || o == nil {
		return err
	}
	if spec.GetControlPlane().GetKubernetesVersion() == "" {
		minor := o.minor
		for _, k := range o.rel.Kubernetes {
			if k.Default {
				minor = k.Minor
			}
		}
		if minor == "" {
			return nil
		}
		if spec.ControlPlane == nil {
			spec.ControlPlane = &cloudv1.ControlPlaneSpec{}
		}
		spec.ControlPlane.KubernetesVersion = minor
		return nil
	}
	return checkOfferedMinor(o, spec.GetControlPlane().GetKubernetesVersion())
}

// CheckReleaseMinor refuses a Kubernetes version the release the cluster
// will use (its channel's, else stable's) does not offer: every platform
// component of the cluster would fail on it. An empty version, or a
// release that cannot be read as offering nothing, passes; a read that
// fails is UNAVAILABLE. The takeover uses it with the version the
// imported control plane runs.
func (s *Reconciler) CheckReleaseMinor(ctx context.Context, spec *cloudv1.ClusterSpec) error {
	version := spec.GetControlPlane().GetKubernetesVersion()
	src := s.releases()
	if version == "" || src == nil {
		return nil
	}
	o, err := readOffer(ctx, src, channelOf(spec), s.logger())
	if err != nil || o == nil {
		return err
	}
	return checkOfferedMinor(o, version)
}

// checkOfferedMinor is nil when the offer's release offers the minor of
// version, else a FAILED_PRECONDITION fault naming the minors it offers.
func checkOfferedMinor(o *channelOffer, version string) error {
	minor := minorString(version)
	if len(o.rel.Kubernetes) == 0 || contains(o.offered(), minor) {
		return nil
	}
	return fault.New(fault.FailedPrecondition, fmt.Sprintf(
		"Kubernetes %s is not offered by release %s of channel %s, which the cluster's platform components come from: it offers %s",
		minor, o.rel.Name, o.channel, strings.Join(o.offered(), ", ")))
}

// CheckChannelVersion refuses an update that moves the Kubernetes
// version past the minor the spec's channel offers (spec is the updated
// one). A cluster whose channel moved below the version it runs is not
// refused: it is never downgraded, it waits for the channel.
func (s *Reconciler) CheckChannelVersion(ctx context.Context, spec *cloudv1.ClusterSpec, versionBefore string) error {
	version := spec.GetControlPlane().GetKubernetesVersion()
	if version == "" || version == versionBefore {
		return nil
	}
	channel, ok := namedChannel(spec)
	src := s.releases()
	if !ok || src == nil {
		return nil
	}
	o, err := readOffer(ctx, src, channel, s.logger())
	if err != nil || o == nil || o.minor == "" {
		return err
	}
	return checkChannelVersion(o, version)
}

// checkChannelVersion is nil for a version the channel's release offers at
// or below the channel's minor; otherwise an INVALID_ARGUMENT fault that
// says what to choose instead.
func checkChannelVersion(o *channelOffer, version string) error {
	minor := minorString(version)
	offered := o.offered()
	if !contains(offered, minor) && !newer(minor, o.minor) {
		return fault.New(fault.Invalid, fmt.Sprintf("Kubernetes %s is not offered: release %s offers %s",
			minor, o.rel.Name, strings.Join(offered, ", ")))
	}
	if !newer(minor, o.minor) {
		return nil
	}
	msg := fmt.Sprintf("channel %s offers Kubernetes %s; choose %s or older", o.channel, o.minor, o.minor)
	var others []string
	for _, ch := range []string{cloudv1alpha1.ChannelStable, cloudv1alpha1.ChannelBeta, cloudv1alpha1.ChannelAlpha} {
		m, ok := release.ChannelMinor(o.rel, ch)
		if ok && ch != o.channel && newer(m, o.minor) {
			others = append(others, fmt.Sprintf("%s (%s)", ch, m))
		}
	}
	if len(others) > 0 {
		msg += ", or channel " + strings.Join(others, " or ")
	}
	return fault.New(fault.Invalid, msg)
}

// kubernetesStatus is what o says of a cluster running current on channel
// ("" when it names none: only ending applies). current is what the
// control plane runs, else the spec's version; "" when neither is known.
func kubernetesStatus(o *channelOffer, channel, current string) *cloudv1.ClusterKubernetes {
	out := &cloudv1.ClusterKubernetes{Channel: channel}
	if channel != "" {
		out.ChannelMinor = o.minor
	}
	if current == "" {
		return out
	}
	minor := minorString(current)
	if out.ChannelMinor != "" {
		next, ok, _ := kubeupgrade.NextHop(minor, out.ChannelMinor, o.offered())
		switch {
		case ok:
			out.Available = next
			out.Message = fmt.Sprintf("upgrade available: %s, the minor channel %s offers", next, channel)
			if next != out.ChannelMinor {
				out.Message = fmt.Sprintf("upgrade available: %s, then %s, the minor channel %s offers (one minor at a time)",
					next, out.ChannelMinor, channel)
			}
		case newer(minor, out.ChannelMinor):
			out.Message = fmt.Sprintf("runs %s, ahead of channel %s's %s: it is never downgraded and waits for the channel",
				minor, channel, out.ChannelMinor)
		}
	}
	if release.Ending(o.rel, minor) {
		out.Ending = true
		out.Message = fmt.Sprintf("Kubernetes %s is on no release channel: it is ending; move to %s", minor, nextOffered(o, minor))
	}
	return out
}

// nextOffered is the minor after minor when the release offers it, else
// "a newer minor".
func nextOffered(o *channelOffer, minor string) string {
	n, ok := minorOf(minor)
	if ok {
		next := fmt.Sprintf("1.%d", n+1)
		if contains(o.offered(), next) {
			return next
		}
	}
	return "a newer minor"
}

// withKubernetes writes into st what the cluster's channel says of the
// version it runs. A channel or release that cannot be read keeps what
// the last pass recorded: a status read does not fail on it.
func (s *Reconciler) withKubernetes(ctx context.Context, rowID string, spec *cloudv1.ClusterSpec, st, recorded *cloudv1.ClusterStatus) {
	src := s.releases()
	if src == nil {
		return
	}
	named, _ := namedChannel(spec)
	o, err := readOffer(ctx, src, channelOf(spec), s.logger().With("cluster_id", rowID))
	if err != nil {
		st.Kubernetes = recorded.GetKubernetes()
		return
	}
	if o == nil {
		st.Kubernetes = nil
		return
	}
	current := st.GetCurrentKubernetesVersion()
	if current == "" {
		current = spec.GetControlPlane().GetKubernetesVersion()
	}
	st.Kubernetes = kubernetesStatus(o, named, current)
}

// minorString is "1.36" of "1.36" and of "1.36.2".
func minorString(version string) string {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// newer reports whether minor a is newer than minor b; false when either
// cannot be read.
func newer(a, b string) bool {
	x, ok1 := minorOf(a)
	y, ok2 := minorOf(b)
	return ok1 && ok2 && x > y
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
