package controller

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Go equivalents of the CRDs' CEL rules (api/v1alpha1/release_types.go),
// for the samples and the shipped defaults.
var (
	releaseName = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[a-z]|shipped-[0-9a-f]{8})$`)
	minorRE     = regexp.MustCompile(`^1\.[0-9]{2}$`)
	uuidRE      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func releaseRules(r *cloudv1alpha1.Release) []string {
	var out []string
	if !releaseName.MatchString(r.Name) {
		out = append(out, "name "+r.Name)
	}
	k := r.Spec.Content.Kubernetes
	if len(k) < 3 || len(k) > 6 {
		out = append(out, "not three to six minors")
	}
	defaults := 0
	for _, m := range k {
		if m.Default {
			defaults++
		}
		if !minorRE.MatchString(m.Minor) || (m.UpgradeTo != "" && !minorRE.MatchString(m.UpgradeTo)) {
			out = append(out, "minor "+m.Minor)
		}
		if m.Channel != "" && !slices.Contains([]string{"alpha", "beta", "stable"}, m.Channel) {
			out = append(out, "channel "+m.Channel)
		}
		for _, c := range m.Components {
			if c.Version == "" {
				out = append(out, "component without a version: "+c.Name)
			}
		}
	}
	if defaults != 1 {
		out = append(out, "not exactly one default")
	}
	if r.Spec.Content.RenderVersion < 1 {
		out = append(out, "renderVersion")
	}
	return out
}

func channelRules(c *cloudv1alpha1.ReleaseChannel) []string {
	var out []string
	if !slices.Contains([]string{"alpha", "beta", "stable"}, c.Name) {
		out = append(out, "name "+c.Name)
	}
	automatic := c.Spec.Promotion == cloudv1alpha1.PromotionAutomatic || c.Spec.Promotion == ""
	if automatic == (c.Spec.Release != "") {
		out = append(out, "release set iff the channel is Pinned or Manual")
	}
	if c.Name == "alpha" && automatic {
		out = append(out, "alpha is automatic")
	}
	return out
}

func holdRules(h *cloudv1alpha1.ReleaseHold) []string {
	var out []string
	if !uuidRE.MatchString(h.Spec.ClusterID) {
		out = append(out, "clusterID "+h.Spec.ClusterID)
	}
	if h.Spec.Release != "" && !releaseName.MatchString(h.Spec.Release) {
		out = append(out, "release "+h.Spec.Release)
	}
	if n := len(h.Spec.Reason); n < 1 || n > 512 {
		out = append(out, "reason")
	}
	return out
}

// readSamples decodes a samples file strictly, one object per document.
func readSamples[T any](t *testing.T, name string) []*T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []*T
	for _, doc := range bytes.Split(raw, []byte("\n---\n")) {
		obj := new(T)
		if err := yaml.UnmarshalStrict(doc, obj); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out = append(out, obj)
	}
	return out
}

// sampleRelease is the name of the release in config/samples: an
// operator's release, named as they name theirs. The shipped release has
// the same content under its own name (DefaultReleaseName).
const sampleRelease = "2026-11-a"

// The admin config in config/samples is what the controller ships, as an
// operator would publish it (their own release name, no shipped label and
// not the shipped notes, so no build ever takes it for its own), plus a
// hold, and every object passes the CRDs' rules.
func TestReleaseSamplesAreTheShippedDefaults(t *testing.T) {
	rel := readSamples[cloudv1alpha1.Release](t, "cloud_v1alpha1_release.yaml")
	if len(rel) != 1 || rel[0].Name != sampleRelease || !equality.Semantic.DeepEqual(rel[0].Spec.Content, DefaultRelease().Spec.Content) {
		t.Errorf("release sample differs from DefaultRelease: %s %+v", rel[0].Name, rel[0].Spec.Content)
	}
	if isShipped(rel[0]) {
		t.Errorf("the release sample would be taken for a shipped release (labels %v, notes %q)", rel[0].Labels, rel[0].Spec.Notes)
	}
	if problems := releaseRules(rel[0]); len(problems) > 0 {
		t.Errorf("release sample: %v", problems)
	}
	if problems := ValidateRelease(rel[0].Spec.Content, crossplaneaws.RenderVersion); len(problems) > 0 {
		t.Errorf("release sample is not valid: %v", problems)
	}
	channels := readSamples[cloudv1alpha1.ReleaseChannel](t, "cloud_v1alpha1_releasechannel.yaml")
	defaults := DefaultChannels()
	if len(channels) != len(defaults) {
		t.Fatalf("%d channel samples, want %d", len(channels), len(defaults))
	}
	for i, ch := range channels {
		if defaults[i].Spec.Release != "" {
			defaults[i].Spec.Release = sampleRelease // alpha is pinned to the sample's release
		}
		if ch.Name != defaults[i].Name || !equality.Semantic.DeepEqual(ch.Spec, defaults[i].Spec) {
			t.Errorf("channel sample %s differs from the shipped %s: %+v", ch.Name, defaults[i].Name, ch.Spec)
		}
		if problems := channelRules(ch); len(problems) > 0 {
			t.Errorf("channel %s: %v", ch.Name, problems)
		}
	}
	holds := readSamples[cloudv1alpha1.ReleaseHold](t, "cloud_v1alpha1_releasehold.yaml")
	if problems := holdRules(holds[0]); len(holds) != 1 || len(problems) > 0 {
		t.Errorf("hold sample: %v", problems)
	}
	if holds[0].Spec.Release != sampleRelease {
		t.Errorf("hold sample names release %s, want the sample's %s", holds[0].Spec.Release, sampleRelease)
	}
}

func TestValidateRelease(t *testing.T) {
	minors := func(m ...cloudv1alpha1.KubernetesRelease) cloudv1alpha1.ReleaseContent {
		return cloudv1alpha1.ReleaseContent{RenderVersion: 1, Kubernetes: m}
	}
	k := func(minor, upgradeTo string) cloudv1alpha1.KubernetesRelease {
		return cloudv1alpha1.KubernetesRelease{Minor: minor, UpgradeTo: upgradeTo}
	}
	on := func(minor, channel string) cloudv1alpha1.KubernetesRelease {
		return cloudv1alpha1.KubernetesRelease{Minor: minor, Channel: channel}
	}
	for name, tt := range map[string]struct {
		content cloudv1alpha1.ReleaseContent
		want    string // a fragment of the problems; "" for none
	}{
		"shipped":                          {DefaultRelease().Spec.Content, ""},
		"consecutive":                      {minors(k("1.35", "1.36"), k("1.36", ""), k("1.37", "")), ""},
		"gap":                              {minors(k("1.33", ""), k("1.36", ""), k("1.37", "")), "not consecutive"},
		"skips a minor":                    {minors(k("1.35", "1.37"), k("1.36", ""), k("1.37", "")), "next minor"},
		"upgrade out of list":              {minors(k("1.35", ""), k("1.36", ""), k("1.37", "1.38")), "does not have"},
		"a channel per minor, one on none": {minors(on("1.38", "alpha"), on("1.37", "beta"), on("1.36", "stable"), on("1.35", "")), ""},
		"stable newer than beta": {minors(on("1.37", "alpha"), on("1.36", "stable"), on("1.35", "beta")),
			"stable (1.36) is newer than beta (1.35)"},
		"a channel on no minor": {minors(on("1.37", "alpha"), on("1.36", "beta"), on("1.35", "")), "no minor is on channel stable"},
		"newer renderer": {cloudv1alpha1.ReleaseContent{RenderVersion: crossplaneaws.RenderVersion + 1,
			Kubernetes: []cloudv1alpha1.KubernetesRelease{k("1.35", ""), k("1.36", ""), k("1.37", "")}}, "newer than this controller"},
	} {
		problems := strings.Join(ValidateRelease(tt.content, crossplaneaws.RenderVersion), "; ")
		if (tt.want == "") != (problems == "") || !strings.Contains(problems, tt.want) {
			t.Errorf("%s: problems %q, want %q", name, problems, tt.want)
		}
	}
}

func releaseClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&cloudv1alpha1.Release{}, &cloudv1alpha1.ReleaseChannel{}, &cloudv1alpha1.ReleaseHold{}).Build()
}

// The shipped release and channels are created when absent, and never
// change an object the operator already has.
func TestCreateReleaseDefaults(t *testing.T) {
	ctx := t.Context()
	beta := DefaultChannels()[1]
	beta.Spec = cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionManual, Release: "2026-10-a"}
	c := releaseClient(t, beta)
	for range 2 {
		if err := CreateReleaseDefaults(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got := &cloudv1alpha1.ReleaseChannel{}
	if err := c.Get(ctx, client.ObjectKey{Name: "beta"}, got); err != nil || got.Spec.Promotion != cloudv1alpha1.PromotionManual {
		t.Errorf("beta = %+v (%v), want the operator's", got.Spec, err)
	}
	for _, name := range []string{"alpha", "stable"} {
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &cloudv1alpha1.ReleaseChannel{}); err != nil {
			t.Errorf("channel %s: %v", name, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKey{Name: DefaultReleaseName}, &cloudv1alpha1.Release{}); err != nil {
		t.Errorf("release: %v", err)
	}
}

func reconcileOnce(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, name string,
) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

// A release is valid or not, keeps a finalizer, and cannot go while a
// channel or a hold names it.
func TestReleaseFinalizerBlocksDeletionWhileNamed(t *testing.T) {
	ctx := t.Context()
	alpha := DefaultChannels()[0]
	c := releaseClient(t, DefaultRelease(), alpha)
	r := &ReleaseReconciler{c: c, render: crossplaneaws.RenderVersion}
	reconcileOnce(t, r, DefaultReleaseName)
	rel := &cloudv1alpha1.Release{}
	if err := c.Get(ctx, client.ObjectKey{Name: DefaultReleaseName}, rel); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rel.Finalizers, cloudv1alpha1.ReleaseFinalizer) ||
		!meta.IsStatusConditionTrue(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid) ||
		!meta.IsStatusConditionTrue(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionInUse) {
		t.Fatalf("release = finalizers %v, conditions %+v", rel.Finalizers, rel.Status.Conditions)
	}
	if err := c.Delete(ctx, rel); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, DefaultReleaseName)
	if err := c.Get(ctx, client.ObjectKey{Name: DefaultReleaseName}, rel); err != nil {
		t.Fatalf("the release went while alpha names it: %v", err)
	}
	if cond := meta.FindStatusCondition(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionInUse); cond == nil ||
		cond.Reason != "DeletionBlocked" || !strings.Contains(cond.Message, "alpha") {
		t.Errorf("InUse = %+v, want the deletion blocked by alpha", cond)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "alpha"}, alpha); err != nil {
		t.Fatal(err)
	}
	alpha.Spec.Release = "2026-12-a"
	if err := c.Update(ctx, alpha); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, DefaultReleaseName)
	if err := c.Get(ctx, client.ObjectKey{Name: DefaultReleaseName}, rel); err == nil {
		t.Error("the release is still there once nothing names it")
	}
}

func TestReleaseInvalidCondition(t *testing.T) {
	rel := DefaultRelease()
	rel.Spec.Content.Kubernetes[2].Minor = "1.30"
	c := releaseClient(t, rel)
	reconcileOnce(t, &ReleaseReconciler{c: c, render: crossplaneaws.RenderVersion}, DefaultReleaseName)
	if err := c.Get(t.Context(), client.ObjectKey{Name: DefaultReleaseName}, rel); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid); cond == nil ||
		cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "not consecutive") {
		t.Errorf("Valid = %+v", cond)
	}
}

// What each channel offers: Pinned/Manual their release, Automatic the
// release its upstream offers; never a release that is not valid or is
// withdrawn.
func TestChannelOffers(t *testing.T) {
	ctx := t.Context()
	valid := DefaultRelease()
	meta.SetStatusCondition(&valid.Status.Conditions, metav1.Condition{Type: cloudv1alpha1.ReleaseConditionValid,
		Status: metav1.ConditionTrue, Reason: "Valid"})
	withdrawn := DefaultRelease()
	withdrawn.Name, withdrawn.Spec.Withdrawn = "2026-12-a", true
	withdrawn.Status = valid.Status
	channels := DefaultChannels()
	c := releaseClient(t, valid, withdrawn, channels[0], channels[1], channels[2])
	at := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	r := &ReleaseChannelReconciler{c: c, now: func() time.Time { return at }}
	minor := map[string]string{}
	offers := func(name string) (string, *metav1.Condition, []cloudv1alpha1.ChannelHistoryEntry) {
		t.Helper()
		reconcileOnce(t, r, name)
		ch := &cloudv1alpha1.ReleaseChannel{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, ch); err != nil {
			t.Fatal(err)
		}
		minor[name] = ch.Status.KubernetesMinor
		return ch.Status.Release, meta.FindStatusCondition(ch.Status.Conditions, cloudv1alpha1.ChannelConditionOffering), ch.Status.History
	}
	defer func() {
		// Each channel reports the minor the shipped release puts on it.
		if want := map[string]string{"alpha": "1.37", "beta": "1.36", "stable": "1.35"}; !maps.Equal(minor, want) {
			t.Errorf("channels' Kubernetes minors %v, want %v", minor, want)
		}
	}()
	if got, cond, history := offers("alpha"); got != DefaultReleaseName || cond.Status != metav1.ConditionTrue || len(history) != 1 {
		t.Errorf("alpha offers %q (%+v, history %d)", got, cond, len(history))
	}
	if got, _, _ := offers("beta"); got != DefaultReleaseName {
		t.Errorf("beta offers %q, want alpha's", got)
	}
	if got, _, _ := offers("stable"); got != DefaultReleaseName {
		t.Errorf("stable offers %q, want beta's", got)
	}
	// Alpha pointed at a withdrawn release keeps offering what it did.
	alpha := &cloudv1alpha1.ReleaseChannel{}
	if err := c.Get(ctx, client.ObjectKey{Name: "alpha"}, alpha); err != nil {
		t.Fatal(err)
	}
	alpha.Spec.Release = withdrawn.Name
	if err := c.Update(ctx, alpha); err != nil {
		t.Fatal(err)
	}
	if got, cond, _ := offers("alpha"); got != DefaultReleaseName || cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "withdrawn") {
		t.Errorf("alpha offers %q with %+v, want the previous release and the reason", got, cond)
	}
}

// A hold resolves its cluster ID to the Cluster object, and says when no
// cluster has it, when its release is missing, and when it expired.
func TestHoldResolvesTheClusterID(t *testing.T) {
	ctx := t.Context()
	const id = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"
	cluster := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "prod-eks",
		Labels: map[string]string{cloudstore.LabelID: id}}}
	hold := func(name, clusterID, release string, until *metav1.Time) *cloudv1alpha1.ReleaseHold {
		return &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudv1alpha1.ReleaseHoldSpec{ClusterID: clusterID, Release: release, Reason: "test", Until: until}}
	}
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	past := metav1.NewTime(now.Add(-time.Hour))
	future := metav1.NewTime(now.Add(time.Hour))
	c := releaseClient(t, cluster, DefaultRelease(),
		hold("ok", id, DefaultReleaseName, &future), hold("lost", "00000000-0000-0000-0000-000000000000", "", nil),
		hold("norelease", id, "2030-01-a", nil), hold("expired", id, "", &past))
	r := &ReleaseHoldReconciler{c: c, now: func() time.Time { return now }}
	for name, want := range map[string]string{"ok": "Resolved", "lost": "ClusterNotFound", "norelease": "ReleaseNotUsable", "expired": "Expired"} {
		res := reconcileOnce(t, r, name)
		got := &cloudv1alpha1.ReleaseHold{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, cloudv1alpha1.HoldConditionResolved)
		if cond == nil || cond.Reason != want {
			t.Errorf("%s: condition %+v, want %s", name, cond, want)
		}
		if name == "ok" && (got.Status.Namespace != "tenant-acme" || got.Status.Name != "prod-eks" || res.RequeueAfter != time.Hour ||
			got.Status.Clusters != 1) {
			t.Errorf("ok: resolved to %s/%s (%d clusters), requeue %s", got.Status.Namespace, got.Status.Name, got.Status.Clusters, res.RequeueAfter)
		}
	}
}

// A hold of a tenant or a channel covers every cluster that matches, those
// added later too: it is resolved with how many match now, none included,
// and names no one cluster.
func TestHoldOfATenantOrChannelCountsItsClusters(t *testing.T) {
	ctx := t.Context()
	cluster := func(name, tenant string, channel cloudv1alpha1.ReleaseChannelName, tier cloudv1alpha1.ClusterTier) *cloudv1alpha1.Cluster {
		return &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-" + tenant, Name: name,
			Labels: map[string]string{cloudstore.LabelID: "id-" + name, cloudstore.LabelTenant: tenant}},
			Spec: cloudv1alpha1.ClusterSpec{ReleaseChannel: channel, Tier: tier}}
	}
	hold := func(name string, spec cloudv1alpha1.ReleaseHoldSpec) *cloudv1alpha1.ReleaseHold {
		spec.Reason = "freeze"
		return &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	}
	c := releaseClient(t,
		cluster("a", "acme", cloudv1alpha1.ReleaseChannelBeta, cloudv1alpha1.ClusterTier0),
		cluster("b", "acme", cloudv1alpha1.ReleaseChannelBeta, cloudv1alpha1.ClusterTier2),
		cluster("c", "globex", cloudv1alpha1.ReleaseChannelStable, ""),
		hold("tenant", cloudv1alpha1.ReleaseHoldSpec{TenantID: "acme"}),
		hold("beta", cloudv1alpha1.ReleaseHoldSpec{Channel: "beta"}),
		hold("beta-tier0", cloudv1alpha1.ReleaseHoldSpec{Channel: "beta", Tier: cloudv1alpha1.ClusterTier0}),
		hold("alpha", cloudv1alpha1.ReleaseHoldSpec{Channel: "alpha"}))
	r := &ReleaseHoldReconciler{c: c, now: time.Now}
	for name, want := range map[string]int32{"tenant": 2, "beta": 2, "beta-tier0": 1, "alpha": 0} {
		reconcileOnce(t, r, name)
		got := &cloudv1alpha1.ReleaseHold{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, cloudv1alpha1.HoldConditionResolved)
		if got.Status.Clusters != want || got.Status.Name != "" || cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("%s: %d clusters (want %d), name %q, condition %+v", name, got.Status.Clusters, want, got.Status.Name, cond)
		}
	}
}
