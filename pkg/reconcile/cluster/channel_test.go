package cluster

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// channelRelease offers 1.37 on alpha, 1.36 on beta, 1.35 on stable and
// 1.34 on no channel (ending).
func channelRelease() *cloudv1alpha1.Release {
	return &cloudv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: "shipped-a"},
		Spec: cloudv1alpha1.ReleaseSpec{Content: cloudv1alpha1.ReleaseContent{Kubernetes: []cloudv1alpha1.KubernetesRelease{
			{Minor: "1.37", Channel: "alpha", Default: true},
			{Minor: "1.36", Channel: "beta"},
			{Minor: "1.35", Channel: "stable"},
			{Minor: "1.34"},
		}}}}
}

// channelSource offers rel on every channel; err fails every read.
type channelSource struct {
	rel *cloudv1alpha1.Release
	err error
}

func (c channelSource) Offered(context.Context, string) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if c.rel == nil {
		return "", nil
	}
	return c.rel.Name, nil
}

func (c channelSource) Release(_ context.Context, name string) (*cloudv1alpha1.Release, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.rel == nil || c.rel.Name != name {
		return nil, nil
	}
	return c.rel, nil
}

var discard = slog.New(slog.DiscardHandler)

func offerOf(t *testing.T, channel string) *channelOffer {
	t.Helper()
	o, err := readOffer(context.Background(), channelSource{rel: channelRelease()}, channel, discard)
	if err != nil || o == nil {
		t.Fatalf("readOffer(%s) = %v, %v", channel, o, err)
	}
	return o
}

func TestCheckChannelVersion(t *testing.T) {
	for _, tc := range []struct {
		name, channel, version string
		want                   string // "" accepts; else a part of the refusal
	}{
		{"the channel's minor", "stable", "1.35", ""},
		{"a patch of it", "stable", "1.35.4", ""},
		{"an older offered minor", "beta", "1.35", ""},
		{"an ending minor", "stable", "1.34", ""},
		{"newer than the channel", "stable", "1.37",
			"channel stable offers Kubernetes 1.35; choose 1.35 or older, or channel beta (1.36) or alpha (1.37)"},
		{"newer than beta", "beta", "1.37", "choose 1.36 or older, or channel alpha (1.37)"},
		{"not offered", "stable", "1.33", "Kubernetes 1.33 is not offered: release shipped-a offers 1.37, 1.36, 1.35, 1.34"},
		{"newer than anything", "alpha", "1.38", "channel alpha offers Kubernetes 1.37; choose 1.37 or older"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkChannelVersion(offerOf(t, tc.channel), tc.version)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want INVALID_ARGUMENT containing %q", err, tc.want)
			}
		})
	}
}

func TestKubernetesStatus(t *testing.T) {
	for _, tc := range []struct {
		name, channel, current string
		want                   *cloudv1.ClusterKubernetes
	}{
		{"on the channel's minor", "beta", "1.36.2", &cloudv1.ClusterKubernetes{Channel: "beta", ChannelMinor: "1.36"}},
		{"behind", "beta", "1.35", &cloudv1.ClusterKubernetes{Channel: "beta", ChannelMinor: "1.36", Available: "1.36",
			Message: "upgrade available: 1.36, the minor channel beta offers"}},
		{"two behind", "alpha", "1.35", &cloudv1.ClusterKubernetes{Channel: "alpha", ChannelMinor: "1.37", Available: "1.36",
			Message: "upgrade available: 1.36, then 1.37, the minor channel alpha offers (one minor at a time)"}},
		{"ahead", "stable", "1.37", &cloudv1.ClusterKubernetes{Channel: "stable", ChannelMinor: "1.35",
			Message: "runs 1.37, ahead of channel stable's 1.35: it is never downgraded and waits for the channel"}},
		{"ending on a channel", "stable", "1.34", &cloudv1.ClusterKubernetes{Channel: "stable", ChannelMinor: "1.35", Available: "1.35", Ending: true,
			Message: "Kubernetes 1.34 is on no release channel: it is ending; move to 1.35"}},
		{"ending on no channel", "", "1.34", &cloudv1.ClusterKubernetes{Ending: true,
			Message: "Kubernetes 1.34 is on no release channel: it is ending; move to 1.35"}},
		{"no channel, not ending", "", "1.36", &cloudv1.ClusterKubernetes{}},
		{"version not known yet", "beta", "", &cloudv1.ClusterKubernetes{Channel: "beta", ChannelMinor: "1.36"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := tc.channel
			if read == "" {
				read = cloudv1alpha1.ChannelStable
			}
			got := kubernetesStatus(offerOf(t, read), tc.channel, tc.current)
			if got.String() != tc.want.String() {
				t.Fatalf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestReadOffer(t *testing.T) {
	ctx := context.Background()
	if o, err := readOffer(ctx, channelSource{}, "stable", discard); o != nil || err != nil {
		t.Fatalf("a channel offering nothing = %v, %v; want nothing to check", o, err)
	}
	_, err := readOffer(ctx, channelSource{err: errors.New("forbidden")}, "stable", discard)
	if !fault.Is(err, fault.Unavailable) || !strings.Contains(err.Error(), "cannot read release channel stable") {
		t.Fatalf("a read failure = %v, want UNAVAILABLE naming the channel", err)
	}
}

// A cluster's status says what its channel offers: here beta (1.36) for a
// cluster on 1.35.
func TestViewReportsTheChannelsMinor(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil, func(d *reconcile.Deps) { d.Releases = channelSource{rel: channelRelease()} })
	row := w.cluster(t)
	spec, err := SpecOf(row)
	if err != nil {
		t.Fatal(err)
	}
	spec.ReleaseChannel = cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA
	spec.ControlPlane.KubernetesVersion = "1.35"
	if row.Spec, err = Encode(spec); err != nil {
		t.Fatal(err)
	}
	c, err := w.svc.View(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	got := c.GetStatus().GetKubernetes()
	if got.GetChannelMinor() != "1.36" || got.GetAvailable() != "1.36" || got.GetEnding() {
		t.Fatalf("status.kubernetes = %v, want channel minor 1.36, available 1.36", got)
	}
}

// A cluster whose minor becomes ending is told once, as a warning.
func TestEndingIsAnEvent(t *testing.T) {
	before := &cloudv1.ClusterStatus{Kubernetes: &cloudv1.ClusterKubernetes{}}
	after := &cloudv1.ClusterStatus{Kubernetes: &cloudv1.ClusterKubernetes{Ending: true, Message: "Kubernetes 1.34 is ending"}}
	var ending []reconcile.Note
	for _, n := range statusEvents(before, after) {
		if n.Reason == reconcile.ReasonKubernetesEnding {
			ending = append(ending, n)
		}
	}
	if len(ending) != 1 || ending[0].Type != reconcile.EventWarning || ending[0].Message != "Kubernetes 1.34 is ending" {
		t.Fatalf("events = %v, want one KubernetesEnding warning", ending)
	}
	for _, n := range statusEvents(after, after) {
		if n.Reason == reconcile.ReasonKubernetesEnding {
			t.Fatalf("a pass that found it ending again emitted %v", n)
		}
	}
}
