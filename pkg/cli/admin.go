package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/duration"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// pauseTarget is what admin pause and resume (a tenant, with one of its
// clusters or networks, or a channel with an optional tier), or admin
// freeze and unfreeze (the platform, a tenant or a channel) name, checked.
type pauseTarget struct {
	channel cloudv1.ReleaseChannel
	tier    cloudv1.ClusterTier
}

// pauseTargetOf checks the target flags of a pause: exactly one of
// --tenant and --channel, --tier only with --channel.
func pauseTargetOf(o Options) (pauseTarget, error) {
	var t pauseTarget
	switch {
	case o.Tenant == "" && o.Channel == "":
		return t, errors.New("--tenant or --channel is required: the tenant (with --cluster or --network for one of its), or the channel's clusters")
	case o.Tenant != "" && o.Channel != "":
		return t, errors.New("--tenant or --channel, not both: a channel's clusters are of every tenant")
	case o.Tier != "" && o.Channel == "":
		return t, errors.New("--tier needs --channel")
	case o.Channel != "" && (o.Cluster != "" || o.PauseNetwork != ""):
		return t, errors.New("--channel takes no --cluster or --network")
	}
	return channelTargetOf(o)
}

// freezeTargetOf checks the target flags of a freeze: --tenant or
// --channel or neither (the platform), --tier only with --channel; and
// at most one of --cluster and --network (let through).
func freezeTargetOf(o Options) (pauseTarget, error) {
	switch {
	case o.Tenant != "" && o.Channel != "":
		return pauseTarget{}, errors.New("--tenant or --channel, not both (neither: the platform)")
	case o.Tier != "" && o.Channel == "":
		return pauseTarget{}, errors.New("--tier needs --channel")
	case o.Cluster != "" && o.PauseNetwork != "":
		return pauseTarget{}, errors.New("--cluster or --network, not both")
	}
	return channelTargetOf(o)
}

// channelTargetOf parses --channel and --tier.
func channelTargetOf(o Options) (pauseTarget, error) {
	var t pauseTarget
	var err error
	if o.Channel != "" {
		if t.channel, err = parseChannel("--channel", o.Channel); err != nil {
			return t, fmt.Errorf("--channel must be alpha, beta or stable, got %q", o.Channel)
		}
	}
	if t.tier, err = parseTier(o.Tier); err != nil {
		return t, err
	}
	return t, nil
}

// text is the target for people: "channel beta tier 0".
func (t pauseTarget) text() string {
	out := "channel " + channelWord(t.channel)
	if t.tier != cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED {
		out += " tier " + tierWord(t.tier)
	}
	return out
}

// parseUntil is --until: RFC 3339, or a date (its start, UTC); nil when
// unset.
func parseUntil(s string) (*timestamppb.Timestamp, error) {
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return timestamppb.New(t), nil
		}
	}
	return nil, fmt.Errorf("--until must be a time (2026-12-31T18:00:00Z) or a date (2026-12-31), got %q", s)
}

// AdminPause calls AdminService.PauseReconcile for --tenant (or only its
// --cluster or --network), or --channel (and --tier), with --reason and
// --until: everything stops (admin freeze stops automatic changes only).
func AdminPause(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	t, err := pauseTargetOf(o)
	if err != nil {
		return nil, err
	}
	until, err := parseUntil(o.Until)
	if err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.PauseReconcileRequest{TenantId: o.Tenant, ClusterId: o.Cluster, NetworkId: o.PauseNetwork,
		Channel: t.channel, Tier: t.tier, Until: until, Reason: o.Reason}, c.Admin.PauseReconcile)
	if err != nil || !isTable(o.Output) {
		return res, err
	}
	r := res.(*cloudv1.PauseReconcileResponse)
	return nil, printPauseResult(o, t, "OBJECTS PAUSED", r.GetResources(), r.GetClusters(), r.GetNote())
}

// AdminResume calls AdminService.ResumeReconcile for the target of the
// pause it ends.
func AdminResume(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	t, err := pauseTargetOf(o)
	if err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.ResumeReconcileRequest{TenantId: o.Tenant, ClusterId: o.Cluster, NetworkId: o.PauseNetwork,
		Channel: t.channel, Tier: t.tier}, c.Admin.ResumeReconcile)
	if err != nil || !isTable(o.Output) {
		return res, err
	}
	r := res.(*cloudv1.ResumeReconcileResponse)
	return nil, printPauseResult(o, t, "OBJECTS RESUMED", r.GetResources(), r.GetClusters(), "")
}

// AdminPauseList calls AdminService.ListPauses.
func AdminPauseList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	res, err := call(ctx, &cloudv1.ListPausesRequest{}, c.Admin.ListPauses)
	if err != nil || !isTable(o.Output) {
		return res, err
	}
	return nil, printPauses(o.Out, res.(*cloudv1.ListPausesResponse).GetPauses())
}

// printPauseResult prints what a pause or resume applied to. A full pause
// of a tenant, network or cluster is printed as it always was (printPause);
// a channel's says its target, what it stops and how many clusters it
// covers.
func printPauseResult(o Options, t pauseTarget, count string, resources, clusters int32, note string) error {
	if t.channel == cloudv1.ReleaseChannel_RELEASE_CHANNEL_UNSPECIFIED {
		return printPause(o, count, resources)
	}
	err := table(o.Out, []string{"TARGET", "WHAT", "CLUSTERS", count},
		[][]string{{t.text(), whatEverything, strconv.Itoa(int(clusters)), strconv.Itoa(int(resources))}})
	if err != nil || note == "" {
		return err
	}
	_, err = fmt.Fprintf(o.Out, "Note: %s\n", note)
	return err
}

// printPause prints what a pause or resume applied to: the scope it was
// asked at, its target and how many managed resources it covered.
func printPause(o Options, count string, resources int32) error {
	scope, target := "tenant", o.Tenant
	switch {
	case o.Cluster != "":
		scope, target = "cluster", o.Cluster
	case o.PauseNetwork != "":
		scope, target = "network", o.PauseNetwork
	}
	return table(o.Out, []string{"TENANT", "SCOPE", "TARGET", count},
		[][]string{{o.Tenant, scope, target, strconv.Itoa(int(resources))}})
}

var pauseHeader = []string{"TARGET", "WHAT", "BY", "SINCE", "UNTIL", "CLUSTERS", "REASON"}

// What a pause stops (admin pause), and a freeze (admin freeze).
const (
	whatEverything       = "everything"
	whatAutomaticChanges = "automatic changes"
)

// printPauses is admin pause list: one row per pause and per freeze.
func printPauses(w io.Writer, pauses []*cloudv1.PauseRecord) error {
	rows := make([][]string, 0, len(pauses))
	for _, p := range pauses {
		what := whatEverything
		if p.GetKind() == cloudv1.PauseKind_PAUSE_KIND_AUTOMATIC_CHANGES {
			what = whatAutomaticChanges
		}
		rows = append(rows, []string{p.GetTarget(), what, orNone(p.GetBy()), age(p.GetSince()), untilText(p.GetUntil()),
			strconv.Itoa(int(p.GetClusters())), orNone(oneLine(p.GetReason()))})
	}
	return table(w, pauseHeader, rows)
}

// untilText is when a pause ends by itself: "in 2d"; <none> without an
// until; "ended" once it has passed.
func untilText(t *timestamppb.Timestamp) string {
	if t == nil || !t.IsValid() {
		return none
	}
	left := t.AsTime().Sub(clock())
	if left <= 0 {
		return "ended"
	}
	return "in " + strings.TrimSpace(duration.HumanDuration(left))
}
