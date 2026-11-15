package controller

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/release"
	"github.com/yindia/cluster-vending/pkg/rollout"
)

// The release controllers keep the status of the operator's release
// objects (docs/design/release-admin.md). They change no cluster: rollouts
// come later.

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=releases;releasechannels,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=releaseholds,verbs=get;list;watch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=releases/status;releasechannels/status;releaseholds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=releases/finalizers,verbs=update

// setupReleases registers the release, channel and hold controllers and
// the creation of the shipped defaults with mgr.
func setupReleases(mgr ctrl.Manager, d Deps) error {
	c := mgr.GetClient()
	log := func(msg string, args ...any) { d.logger().Error(msg, args...) }
	if err := mgr.Add(releaseDefaults{c: c, log: log}); err != nil {
		return err
	}
	all := func(list client.ObjectList) handler.EventHandler {
		return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
			return enqueueAll(ctx, c, list)
		})
	}
	err := ctrl.NewControllerManagedBy(mgr).Named("release").For(&cloudv1alpha1.Release{}).
		Watches(&cloudv1alpha1.ReleaseChannel{}, all(&cloudv1alpha1.ReleaseList{})).
		Watches(&cloudv1alpha1.ReleaseHold{}, all(&cloudv1alpha1.ReleaseList{})).
		Watches(&cloudv1alpha1.ComponentDefinition{}, all(&cloudv1alpha1.ReleaseList{})).
		Complete(&ReleaseReconciler{c: c, render: crossplaneaws.RenderVersion, events: d.Events})
	if err != nil {
		return err
	}
	err = ctrl.NewControllerManagedBy(mgr).Named("releasechannel").For(&cloudv1alpha1.ReleaseChannel{}).
		Watches(&cloudv1alpha1.Release{}, all(&cloudv1alpha1.ReleaseChannelList{})).
		Watches(&cloudv1alpha1.ComponentDefinition{}, all(&cloudv1alpha1.ReleaseChannelList{})).
		Complete(&ReleaseChannelReconciler{c: c, events: d.Events})
	if err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).Named("releasehold").For(&cloudv1alpha1.ReleaseHold{}).
		Watches(&cloudv1alpha1.Release{}, all(&cloudv1alpha1.ReleaseHoldList{})).
		Watches(&cloudv1alpha1.Cluster{}, all(&cloudv1alpha1.ReleaseHoldList{})).
		Complete(&ReleaseHoldReconciler{c: c})
}

// enqueueAll is a request for every object of a cluster-scoped list: the
// release objects are few, and each depends on the others.
func enqueueAll(ctx context.Context, c client.Client, list client.ObjectList) []reconcile.Request {
	if err := c.List(ctx, list); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(items))
	for _, item := range items {
		if obj, ok := item.(client.Object); ok {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
		}
	}
	return out
}

// ReleaseReconciler keeps a Release's status: whether it is valid, which
// channels offer it, which holds name it; and keeps it while it is named.
type ReleaseReconciler struct {
	c client.Client
	// render is this build's renderer version (crossplaneaws.RenderVersion).
	render int64
	// events is where a release becoming valid or invalid is reported;
	// nil reports nothing.
	events platform.Events
}

// Reconcile computes the release's status, and lets it be deleted only
// once no channel and no hold names it.
func (r *ReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rel := &cloudv1alpha1.Release{}
	if err := r.c.Get(ctx, req.NamespacedName, rel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	channels, holds, err := r.namedBy(ctx, rel.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	inUse := len(channels.names) > 0 || len(holds) > 0
	if !rel.DeletionTimestamp.IsZero() && !inUse {
		if controllerutil.RemoveFinalizer(rel, cloudv1alpha1.ReleaseFinalizer) {
			return ctrl.Result{}, client.IgnoreNotFound(r.c.Update(ctx, rel))
		}
		return ctrl.Result{}, nil
	}
	if rel.DeletionTimestamp.IsZero() && controllerutil.AddFinalizer(rel, cloudv1alpha1.ReleaseFinalizer) {
		if err := r.c.Update(ctx, rel); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	status := *rel.Status.DeepCopy()
	status.Channels, status.Holds = channels.offering, holds
	catalog, err := listCatalog(ctx, r.c)
	if err != nil {
		return ctrl.Result{}, err
	}
	problems := ValidateRelease(rel.Spec.Content, r.render)
	problems = append(problems, release.CheckGroups(releasePins(rel.Name, rel.Spec.Content), catalog)...)
	problems = append(problems, release.CheckBundles(releasePins(rel.Name, rel.Spec.Content), catalog)...)
	setValid(&status.Conditions, rel.Generation, problems)
	setInUse(&status.Conditions, rel.Generation, rel.DeletionTimestamp.IsZero(), channels.names, holds)
	if equality.Semantic.DeepEqual(status, rel.Status) {
		return ctrl.Result{}, nil
	}
	was := meta.FindStatusCondition(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid)
	rel.Status = status
	if err := r.c.Status().Update(ctx, rel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if n := releaseEvent(was, meta.FindStatusCondition(status.Conditions, cloudv1alpha1.ReleaseConditionValid)); n != nil && r.events != nil {
		r.events.Event(ctx, platform.Object{Kind: platform.KindRelease, Name: rel.Name}, n.Type, n.Reason, n.Message)
	}
	return ctrl.Result{}, nil
}

// releaseEvent says what a change of a release's Valid condition means: it
// can be offered, or it cannot and why. Nil when nothing changed.
func releaseEvent(was, is *metav1.Condition) *platform.Note {
	switch {
	case is == nil:
		return nil
	case is.Status == metav1.ConditionTrue && (was == nil || was.Status != metav1.ConditionTrue):
		n := platform.Normal(platform.ReasonReleaseValid, "the release passed its checks and can be offered")
		return &n
	case is.Status == metav1.ConditionFalse && (was == nil || was.Status != metav1.ConditionFalse || was.Message != is.Message):
		n := platform.Warning(platform.ReasonReleaseInvalid, "the release cannot be offered: "+is.Message)
		return &n
	default:
		return nil
	}
}

// channelEvents say what changed about a channel: the release it offers,
// and a release it should offer but cannot (a new reason is news; the same
// one again is not).
func channelEvents(was, is cloudv1alpha1.ReleaseChannelStatus) []platform.Note {
	var out []platform.Note
	offering := meta.FindStatusCondition(is.Conditions, cloudv1alpha1.ChannelConditionOffering)
	if is.Release != "" && is.Release != was.Release {
		msg := "offers " + is.Release
		if was.Release != "" {
			msg += " (before: " + was.Release + ")"
		}
		out = append(out, platform.Normal(platform.ReasonChannelOffers, msg))
	}
	before := meta.FindStatusCondition(was.Conditions, cloudv1alpha1.ChannelConditionOffering)
	if offering != nil && offering.Status == metav1.ConditionFalse &&
		(before == nil || before.Status != metav1.ConditionFalse || before.Message != offering.Message) {
		out = append(out, platform.Warning(platform.ReasonChannelCannotMove, offering.Message))
	}
	if p := is.Pending; p != nil && (was.Pending == nil || was.Pending.Release != p.Release) {
		out = append(out, platform.Normal(platform.ReasonPromotionWaiting, fmt.Sprintf(
			"waiting to take %s once its release soak ends (%s)", p.Release, p.Until.UTC().Format(time.RFC3339))))
	}
	return out
}

// namedRelease is who names a release: channels naming it (spec.release)
// or offering it (status.release), and among them those offering it.
type namedRelease struct {
	names, offering []string
}

// namedBy lists the channels and holds that name release.
func (r *ReleaseReconciler) namedBy(ctx context.Context, release string) (namedRelease, []string, error) {
	var out namedRelease
	channels := &cloudv1alpha1.ReleaseChannelList{}
	if err := r.c.List(ctx, channels); err != nil {
		return out, nil, fmt.Errorf("list release channels: %w", err)
	}
	for _, ch := range channels.Items {
		if ch.Status.Release == release {
			out.offering = append(out.offering, ch.Name)
		}
		if ch.Spec.Release == release || ch.Status.Release == release {
			out.names = append(out.names, ch.Name)
		}
	}
	holds := &cloudv1alpha1.ReleaseHoldList{}
	if err := r.c.List(ctx, holds); err != nil {
		return out, nil, fmt.Errorf("list release holds: %w", err)
	}
	var held []string
	for _, h := range holds.Items {
		if h.Spec.Release == release {
			held = append(held, h.Name)
		}
	}
	slices.Sort(out.names)
	slices.Sort(out.offering)
	slices.Sort(held)
	return out, held, nil
}

// ValidateRelease makes the checks of a release's content the API
// server's CEL rules cannot: the minors are consecutive, an upgradeTo is
// the next minor and one of them, the channels are each on one minor
// from the newest (alpha) to the oldest (stable) or none is
// (release.ChannelProblems), and the renderer version is not newer than
// this build's (render). It returns the problems found; none means valid.
func ValidateRelease(content cloudv1alpha1.ReleaseContent, render int64) []string {
	var problems []string
	if content.RenderVersion > render {
		problems = append(problems, fmt.Sprintf("renderVersion %d is newer than this controller's renderer (%d)", content.RenderVersion, render))
	}
	minors := map[string]bool{}
	var numbers []int
	for _, k := range content.Kubernetes {
		n, ok := minorNumber(k.Minor)
		if !ok {
			problems = append(problems, fmt.Sprintf("minor %q is not 1.<n>", k.Minor))
			continue
		}
		minors[k.Minor] = true
		numbers = append(numbers, n)
	}
	slices.Sort(numbers)
	for i := 1; i < len(numbers); i++ {
		if numbers[i] != numbers[i-1]+1 {
			problems = append(problems, fmt.Sprintf("the minors are not consecutive: 1.%d is followed by 1.%d", numbers[i-1], numbers[i]))
		}
	}
	for _, k := range content.Kubernetes {
		if k.UpgradeTo == "" {
			continue
		}
		from, okFrom := minorNumber(k.Minor)
		to, okTo := minorNumber(k.UpgradeTo)
		switch {
		case !okFrom || !okTo || to != from+1:
			problems = append(problems, fmt.Sprintf("%s upgrades to %s: an upgrade goes to the next minor", k.Minor, k.UpgradeTo))
		case !minors[k.UpgradeTo]:
			problems = append(problems, fmt.Sprintf("%s upgrades to %s, which the release does not have", k.Minor, k.UpgradeTo))
		}
	}
	problems = append(problems, release.ChannelProblems(releasePins("", content))...)
	return append(problems, pinValueProblems(content)...)
}

// minorNumber is n of "1.<n>".
func minorNumber(minor string) (int, bool) {
	rest, ok := strings.CutPrefix(minor, "1.")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

// setValid sets the Valid condition from the problems found.
func setValid(conds *[]metav1.Condition, generation int64, problems []string) {
	c := metav1.Condition{Type: cloudv1alpha1.ReleaseConditionValid, Status: metav1.ConditionTrue, Reason: "Valid",
		Message: "the release can be offered", ObservedGeneration: generation}
	if len(problems) > 0 {
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Invalid", strings.Join(problems, "; ")
	}
	meta.SetStatusCondition(conds, c)
}

// setInUse sets the InUse condition: who names the release, and on a
// release being deleted, that the deletion waits for them.
func setInUse(conds *[]metav1.Condition, generation int64, live bool, channels, holds []string) {
	if len(channels) == 0 && len(holds) == 0 {
		meta.SetStatusCondition(conds, metav1.Condition{Type: cloudv1alpha1.ReleaseConditionInUse, Status: metav1.ConditionFalse,
			Reason: "Unused", Message: "no channel and no hold names the release", ObservedGeneration: generation})
		return
	}
	var by []string
	if len(channels) > 0 {
		by = append(by, "channels "+strings.Join(channels, ", "))
	}
	if len(holds) > 0 {
		by = append(by, "holds "+strings.Join(holds, ", "))
	}
	msg, reason := "named by "+strings.Join(by, " and "), "InUse"
	if !live {
		msg, reason = "cannot be deleted while it is "+msg+": point them at another release first", "DeletionBlocked"
	}
	meta.SetStatusCondition(conds, metav1.Condition{Type: cloudv1alpha1.ReleaseConditionInUse, Status: metav1.ConditionTrue,
		Reason: reason, Message: msg, ObservedGeneration: generation})
}

// ReleaseChannelReconciler keeps a channel's status: the release it offers
// and its history.
type ReleaseChannelReconciler struct {
	c   client.Client
	now func() time.Time
	// events is where the channel offering another release, or not being
	// able to, is reported; nil reports nothing.
	events platform.Events
}

// upstream is the channel a channel follows: beta follows alpha, stable
// follows beta; alpha follows none.
func upstream(channel string) string {
	switch channel {
	case cloudv1alpha1.ChannelBeta:
		return cloudv1alpha1.ChannelAlpha
	case cloudv1alpha1.ChannelStable:
		return cloudv1alpha1.ChannelBeta
	}
	return ""
}

// Reconcile resolves the release the channel offers. Pinned and Manual
// offer spec.release; Automatic offers the release its upstream offers.
// Until rollouts exist (docs/design/release-admin.md, step 6), a release
// counts as completed upstream as soon as the upstream offers it. A
// release that is missing, invalid or withdrawn is not offered: the
// channel keeps offering what it did, with the Offering condition saying
// why. No cluster is moved.
func (r *ReleaseChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ch := &cloudv1alpha1.ReleaseChannel{}
	if err := r.c.Get(ctx, req.NamespacedName, ch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	want, why, wait, err := r.target(ctx, ch)
	if err != nil {
		return ctrl.Result{}, err
	}
	status := *ch.Status.DeepCopy()
	status.Pending = wait.pending
	offering := metav1.Condition{Type: cloudv1alpha1.ChannelConditionOffering, ObservedGeneration: ch.Generation}
	problem := r.unusable(ctx, want, why)
	if problem == "" {
		if problem, err = stepProblem(ctx, r.c, status.Release, want); err != nil {
			return ctrl.Result{}, err
		}
	}
	switch {
	case problem != "":
		offering.Status, offering.Reason, offering.Message = metav1.ConditionFalse, "NotOffered", problem
	default:
		if status.Release != want {
			status.History = recordOffer(status.History, want, why, r.clock())
		}
		status.Release = want
		offering.Status, offering.Reason = metav1.ConditionTrue, "Offering"
		offering.Message = fmt.Sprintf("offers %s (%s)", want, why)
	}
	if status.Release != "" && offering.Status == metav1.ConditionFalse {
		offering.Message += "; still offers " + status.Release
	}
	if p := status.Pending; p != nil {
		offering.Message += fmt.Sprintf("; waiting to take %s: its release soak ends in %s (%s)",
			p.Release, shortDuration(p.Until.Sub(r.clock().Time)), p.Until.UTC().Format(time.RFC3339))
	}
	if status.KubernetesMinor, err = r.channelMinor(ctx, status.Release, ch.Name); err != nil {
		return ctrl.Result{}, err
	}
	meta.SetStatusCondition(&status.Conditions, offering)
	res := ctrl.Result{RequeueAfter: wait.after}
	if !equality.Semantic.DeepEqual(status, ch.Status) {
		was := ch.Status
		ch.Status = status
		if err := r.c.Status().Update(ctx, ch); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if r.events != nil {
			for _, n := range channelEvents(was, status) {
				r.events.Event(ctx, platform.Object{Kind: platform.KindReleaseChannel, Name: ch.Name}, n.Type, n.Reason, n.Message)
			}
		}
	}
	return res, nil
}

// soakWait is an Automatic channel waiting out its release soak: the
// release it takes next, and when to look again.
type soakWait struct {
	pending *cloudv1alpha1.PendingRelease
	after   time.Duration
}

// target is the release the channel should offer and why ("" with the
// reason when there is none). An Automatic channel takes its upstream's
// release once the upstream has offered it for the release soak; until
// then it keeps its own and wait says what it waits for. A channel that
// offers nothing yet takes it at once.
func (r *ReleaseChannelReconciler) target(ctx context.Context, ch *cloudv1alpha1.ReleaseChannel) (string, string, soakWait, error) {
	promotion := ch.Spec.Promotion
	if promotion == "" {
		promotion = cloudv1alpha1.PromotionAutomatic
	}
	if promotion != cloudv1alpha1.PromotionAutomatic {
		return ch.Spec.Release, string(promotion), soakWait{}, nil
	}
	up := upstream(ch.Name)
	if up == "" {
		return "", "alpha has no upstream channel: make it Pinned or Manual to a release", soakWait{}, nil
	}
	upper := &cloudv1alpha1.ReleaseChannel{}
	if err := r.c.Get(ctx, client.ObjectKey{Name: up}, upper); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", fmt.Sprintf("its upstream channel %s does not exist", up), soakWait{}, nil
		}
		return "", "", soakWait{}, err
	}
	next := upper.Status.Release
	if next == "" {
		return "", fmt.Sprintf("its upstream channel %s offers no release yet", up), soakWait{}, nil
	}
	why := "Automatic: follows " + up
	if ch.Status.Release == "" || ch.Status.Release == next {
		return next, why, soakWait{}, nil
	}
	since := offeredSince(upper, r.clock().Time)
	until := since.Add(releaseSoak(ch))
	if now := r.clock().Time; now.Before(until) {
		return ch.Status.Release, why, soakWait{after: until.Sub(now), pending: &cloudv1alpha1.PendingRelease{
			Release: next, Since: metav1.NewTime(since), Until: metav1.NewTime(until)}}, nil
	}
	return next, fmt.Sprintf("%s, after its release soak of %s", why, shortDuration(releaseSoak(ch))), soakWait{}, nil
}

// Release soaks of an Automatic channel that sets none.
const (
	defaultBetaReleaseSoak   = 7 * 24 * time.Hour
	defaultStableReleaseSoak = 14 * 24 * time.Hour
)

// releaseSoak is how long the channel's upstream must have offered a
// release before the channel takes it: spec.policy.releaseSoak, else 7
// days for beta and 14 for stable.
func releaseSoak(ch *cloudv1alpha1.ReleaseChannel) time.Duration {
	if s := ch.Spec.Policy.ReleaseSoak; s != nil {
		return s.Duration
	}
	if ch.Name == cloudv1alpha1.ChannelStable {
		return defaultStableReleaseSoak
	}
	return defaultBetaReleaseSoak
}

// offeredSince is when the channel started offering its release: its
// history entry, else its Offering condition's last change, else now.
func offeredSince(ch *cloudv1alpha1.ReleaseChannel, now time.Time) time.Time {
	for _, h := range ch.Status.History {
		if h.Release == ch.Status.Release && h.To == nil {
			return h.From.Time
		}
	}
	if c := meta.FindStatusCondition(ch.Status.Conditions, cloudv1alpha1.ChannelConditionOffering); c != nil && !c.LastTransitionTime.IsZero() {
		return c.LastTransitionTime.Time
	}
	return now
}

// shortDuration is d in days, hours or minutes, rounded: "7d", "3h", "12m".
func shortDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Round(time.Hour).Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Round(time.Minute).Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute).Minutes()))
	}
}

// unusable says why release cannot be offered; "" when it can. why is the
// reason there is none when release is "".
func (r *ReleaseChannelReconciler) unusable(ctx context.Context, release, why string) string {
	if release == "" {
		return why
	}
	rel := &cloudv1alpha1.Release{}
	if err := r.c.Get(ctx, client.ObjectKey{Name: release}, rel); err != nil {
		return fmt.Sprintf("release %s cannot be read: %v", release, err)
	}
	switch {
	case rel.Spec.Withdrawn:
		return fmt.Sprintf("release %s is withdrawn", release)
	case !rel.DeletionTimestamp.IsZero():
		return fmt.Sprintf("release %s is being deleted", release)
	case !meta.IsStatusConditionTrue(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid):
		return fmt.Sprintf("release %s is not valid (yet): %s", release, conditionMessage(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid))
	}
	return ""
}

// channelMinor is the Kubernetes minor the release a channel offers puts
// on that channel; "" when it offers none, the release is gone, or it puts
// no minor on the channel.
func (r *ReleaseChannelReconciler) channelMinor(ctx context.Context, name, channel string) (string, error) {
	if name == "" {
		return "", nil
	}
	rel := &cloudv1alpha1.Release{}
	if err := r.c.Get(ctx, client.ObjectKey{Name: name}, rel); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	minor, _ := release.ChannelMinor(releasePins(rel.Name, rel.Spec.Content), channel)
	return minor, nil
}

func (r *ReleaseChannelReconciler) clock() metav1.Time {
	if r.now != nil {
		return metav1.NewTime(r.now())
	}
	return metav1.Now()
}

// maxHistory is how many releases a channel's history keeps.
const maxHistory = 20

// recordOffer closes the history entry of the release offered until now
// and starts one for release, newest first, at most maxHistory.
func recordOffer(history []cloudv1alpha1.ChannelHistoryEntry, release, why string, at metav1.Time) []cloudv1alpha1.ChannelHistoryEntry {
	out := make([]cloudv1alpha1.ChannelHistoryEntry, 0, len(history)+1)
	out = append(out, cloudv1alpha1.ChannelHistoryEntry{Release: release, From: at, Reason: why})
	for i, h := range history {
		if i == 0 && h.To == nil {
			h.To = &at
		}
		out = append(out, h)
	}
	if len(out) > maxHistory {
		out = out[:maxHistory]
	}
	return out
}

// conditionMessage is the message of a condition, or "not checked yet".
func conditionMessage(conds []metav1.Condition, typ string) string {
	if c := meta.FindStatusCondition(conds, typ); c != nil {
		return c.Message
	}
	return "not checked yet"
}

// ReleaseHoldReconciler resolves a hold's cluster ID to its Cluster object
// and checks the release it names.
type ReleaseHoldReconciler struct {
	c   client.Client
	now func() time.Time
}

// Reconcile writes the namespace and name the hold's cluster ID resolves
// to, and the Resolved condition: no cluster with that ID, a release that
// is missing or withdrawn, and a hold whose until has passed are reported.
// It changes no cluster.
func (r *ReleaseHoldReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hold := &cloudv1alpha1.ReleaseHold{}
	if err := r.c.Get(ctx, req.NamespacedName, hold); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	clusters := &cloudv1alpha1.ClusterList{}
	var opts []client.ListOption
	if hold.Spec.ClusterID != "" {
		opts = append(opts, client.MatchingLabels{cloudstore.LabelID: hold.Spec.ClusterID})
	}
	if err := r.c.List(ctx, clusters, opts...); err != nil {
		return ctrl.Result{}, fmt.Errorf("list clusters: %w", err)
	}
	matched := slices.DeleteFunc(clusters.Items, func(c cloudv1alpha1.Cluster) bool { return !rollout.Matches(hold.Spec, &c) })
	status := *hold.Status.DeepCopy()
	status.Namespace, status.Name, status.Clusters = "", "", int32(min(len(matched), math.MaxInt32))
	resolved := metav1.Condition{Type: cloudv1alpha1.HoldConditionResolved, Status: metav1.ConditionTrue,
		Reason: "Resolved", ObservedGeneration: hold.Generation}
	var requeue time.Duration
	switch {
	case hold.Spec.ClusterID != "" && len(matched) == 0:
		resolved.Status, resolved.Reason = metav1.ConditionFalse, "ClusterNotFound"
		resolved.Message = fmt.Sprintf("no cluster has ID %s", hold.Spec.ClusterID)
	default:
		if hold.Spec.ClusterID != "" {
			cl := matched[0]
			status.Namespace, status.Name = cl.Namespace, cl.Name
			resolved.Message = fmt.Sprintf("holds cluster %s/%s", cl.Namespace, cl.Name)
		} else {
			// A tenant's or a channel's: the clusters that match now; one
			// added later is covered too.
			resolved.Message = fmt.Sprintf("pauses the upgrades of %d clusters (%s)", len(matched), rollout.HoldTarget(hold.Spec))
		}
		if problem := r.releaseProblem(ctx, hold.Spec.Release); problem != "" {
			resolved.Status, resolved.Reason, resolved.Message = metav1.ConditionFalse, "ReleaseNotUsable", problem
		} else if until := hold.Spec.Until; until != nil {
			if left := until.Sub(r.clock()); left <= 0 {
				resolved.Status, resolved.Reason = metav1.ConditionFalse, "Expired"
				resolved.Message = fmt.Sprintf("the hold ended at %s; delete it", until.UTC().Format(time.RFC3339))
			} else {
				requeue = left
			}
		}
	}
	meta.SetStatusCondition(&status.Conditions, resolved)
	if !equality.Semantic.DeepEqual(status, hold.Status) {
		hold.Status = status
		if err := r.c.Status().Update(ctx, hold); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// releaseProblem says why the hold's release cannot be used; "" for none
// named or a usable one.
func (r *ReleaseHoldReconciler) releaseProblem(ctx context.Context, release string) string {
	if release == "" {
		return ""
	}
	rel := &cloudv1alpha1.Release{}
	if err := r.c.Get(ctx, client.ObjectKey{Name: release}, rel); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return fmt.Sprintf("release %s does not exist", release)
		}
		return fmt.Sprintf("release %s cannot be read: %v", release, err)
	}
	if rel.Spec.Withdrawn {
		return fmt.Sprintf("release %s is withdrawn", release)
	}
	return ""
}

func (r *ReleaseHoldReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
