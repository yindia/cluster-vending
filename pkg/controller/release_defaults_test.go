package controller

import (
	"regexp"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// The shipped release is named after its content: a build that ships other
// content ships another release, since a release's content cannot change.
func TestShippedReleaseNameFollowsItsContent(t *testing.T) {
	base := DefaultRelease().Spec.Content
	tests := []struct {
		name string
		edit func(*cloudv1alpha1.ReleaseContent)
		same bool
	}{
		{name: "the same content", edit: func(*cloudv1alpha1.ReleaseContent) {}, same: true},
		{name: "another chart version", edit: func(c *cloudv1alpha1.ReleaseContent) { c.Kubernetes[0].Components[0].Version = "9.9.9" }},
		{name: "a pin less", edit: func(c *cloudv1alpha1.ReleaseContent) { c.Kubernetes[2].Components = c.Kubernetes[2].Components[1:] }},
		{name: "another renderer", edit: func(c *cloudv1alpha1.ReleaseContent) { c.RenderVersion++ }},
		{name: "another end of support", edit: func(c *cloudv1alpha1.ReleaseContent) {
			c.Kubernetes[1].EndOfSupport = metav1.NewTime(c.Kubernetes[1].EndOfSupport.AddDate(0, 1, 0))
		}},
	}
	shipped := regexp.MustCompile(`^shipped-[0-9a-f]{8}$`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			content := *base.DeepCopy()
			tt.edit(&content)

			// Act
			got := ShippedReleaseName(content)

			// Assert
			if !shipped.MatchString(got) {
				t.Errorf("name %q is not shipped-<8 hex>", got)
			}
			if (got == DefaultReleaseName) != tt.same {
				t.Errorf("name %q, the shipped release is %q: same = %v, want %v", got, DefaultReleaseName, got == DefaultReleaseName, tt.same)
			}
		})
	}
	if rel := DefaultRelease(); rel.Name != DefaultReleaseName || rel.Labels[cloudv1alpha1.LabelShipped] != "true" {
		t.Errorf("DefaultRelease is %q with labels %v", rel.Name, rel.Labels)
	}
}

// A new build moves alpha to its shipped release only while alpha is as a
// build left it: pinned to a shipped release, nothing else set. A release
// the operator published and a channel the operator edited stay.
func TestShippedReleaseTakesOverAlpha(t *testing.T) {
	const old, legacy, own = "shipped-0badc0de", "2026-11-a", "2026-10-b"
	release := func(name string, shipped bool, notes string) *cloudv1alpha1.Release {
		rel := DefaultRelease()
		rel.Name, rel.Labels, rel.Spec.Notes = name, nil, notes
		rel.Spec.Content.Kubernetes[0].Components = nil // an older build's content
		if shipped {
			rel.Labels = map[string]string{cloudv1alpha1.LabelShipped: "true"}
		}
		return rel
	}
	alpha := func(spec cloudv1alpha1.ReleaseChannelSpec) *cloudv1alpha1.ReleaseChannel {
		ch := DefaultChannels()[0]
		ch.Spec = spec
		return ch
	}
	pinned := func(name string) cloudv1alpha1.ReleaseChannelSpec {
		return cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionPinned, Release: name}
	}
	paused := pinned(old)
	paused.Paused = true
	// What the API server makes of a channel created without a policy.
	filledIn := pinned(old)
	filledIn.Policy = cloudv1alpha1.RolloutPolicy{MaxInProgress: 5, MaxFailed: 1, Soak: metav1.Duration{Duration: 24 * time.Hour}}
	ownPolicy := pinned(old)
	ownPolicy.Policy = cloudv1alpha1.RolloutPolicy{MaxInProgress: 2, MaxFailed: 1, Soak: metav1.Duration{Duration: 24 * time.Hour}}
	tests := []struct {
		name    string
		objects []client.Object
		want    string // the release alpha names afterwards
		kept    string // a release that must still exist
	}{
		{name: "a fresh install", want: DefaultReleaseName},
		{name: "alpha on an older build's release", objects: []client.Object{release(old, true, ""), alpha(pinned(old))},
			want: DefaultReleaseName, kept: old},
		{name: "alpha on the release builds shipped before releases were named after their content",
			objects: []client.Object{release(legacy, false, shippedNotes), alpha(pinned(legacy))}, want: DefaultReleaseName, kept: legacy},
		{name: "alpha on a release the operator published", objects: []client.Object{release(own, false, "ours"), alpha(pinned(own))},
			want: own, kept: own},
		// The shipped content and notes, republished by hand under another
		// name: the operator's, whatever its notes say.
		{name: "the shipped sample published by the operator under its own name",
			objects: []client.Object{release("2026-11-b", false, shippedNotes), alpha(pinned("2026-11-b"))}, want: "2026-11-b", kept: "2026-11-b"},
		{name: "a release named like the old shipped one, published by the operator",
			objects: []client.Object{release(legacy, false, "ours"), alpha(pinned(legacy))}, want: legacy, kept: legacy},
		{name: "alpha made Manual by the operator", objects: []client.Object{release(old, true, ""),
			alpha(cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionManual, Release: old})}, want: old, kept: old},
		{name: "alpha paused by the operator", objects: []client.Object{release(old, true, ""), alpha(paused)}, want: old, kept: old},
		{name: "alpha with the policy defaults the API server fills in", objects: []client.Object{release(old, true, ""), alpha(filledIn)},
			want: DefaultReleaseName, kept: old},
		{name: "alpha with a rollout policy of the operator", objects: []client.Object{release(old, true, ""), alpha(ownPolicy)}, want: old, kept: old},
		{name: "alpha names a release that is gone", objects: []client.Object{alpha(pinned(old))}, want: old},
		{name: "alpha already on this build's release", objects: []client.Object{alpha(pinned(DefaultReleaseName))}, want: DefaultReleaseName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			ctx := t.Context()
			c := releaseClient(t, tt.objects...)

			// Act: twice, a restart changes nothing more.
			for range 2 {
				if err := CreateReleaseDefaults(ctx, c); err != nil {
					t.Fatal(err)
				}
			}

			// Assert
			got := &cloudv1alpha1.ReleaseChannel{}
			if err := c.Get(ctx, client.ObjectKey{Name: cloudv1alpha1.ChannelAlpha}, got); err != nil {
				t.Fatal(err)
			}
			if got.Spec.Release != tt.want {
				t.Errorf("alpha names %q, want %q", got.Spec.Release, tt.want)
			}
			shipped := &cloudv1alpha1.Release{}
			if err := c.Get(ctx, client.ObjectKey{Name: DefaultReleaseName}, shipped); err != nil {
				t.Errorf("this build's release: %v", err)
			}
			if tt.kept != "" {
				if err := c.Get(ctx, client.ObjectKey{Name: tt.kept}, &cloudv1alpha1.Release{}); err != nil {
					t.Errorf("release %s must stay: %v", tt.kept, err)
				}
			}
			for _, name := range []string{cloudv1alpha1.ChannelBeta, cloudv1alpha1.ChannelStable} {
				if err := c.Get(ctx, client.ObjectKey{Name: name}, &cloudv1alpha1.ReleaseChannel{}); err != nil {
					t.Errorf("channel %s: %v", name, err)
				}
			}
		})
	}
}
