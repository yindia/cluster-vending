package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// DefaultReleaseName is the release this build ships.
const DefaultReleaseName = "2026-11-a"

// date is midnight UTC of a day, for the shipped end-of-support dates.
func date(year int, month time.Month, day int) metav1.Time {
	return metav1.NewTime(time.Date(year, month, day, 0, 0, 0, 0, time.UTC))
}

// DefaultRelease is the release this build ships: the three newest
// Kubernetes minors, the newest the default, each older one upgrading to
// the next, and this build's renderer. No platform components yet. The
// end-of-support dates are placeholders for the operator to replace by
// publishing a release of their own.
func DefaultRelease() *cloudv1alpha1.Release {
	return &cloudv1alpha1.Release{
		TypeMeta:   metav1.TypeMeta{APIVersion: cloudv1alpha1.GroupVersion.String(), Kind: "Release"},
		ObjectMeta: metav1.ObjectMeta{Name: DefaultReleaseName},
		Spec: cloudv1alpha1.ReleaseSpec{
			Notes: "Shipped with the controller. End-of-support dates are placeholders: publish your own release.",
			Content: cloudv1alpha1.ReleaseContent{
				RenderVersion: crossplaneaws.RenderVersion,
				Kubernetes: []cloudv1alpha1.KubernetesRelease{
					{Minor: "1.37", Default: true, EndOfSupport: date(2028, time.January, 31)},
					{Minor: "1.36", UpgradeTo: "1.37", EndOfSupport: date(2027, time.September, 30)},
					{Minor: "1.35", UpgradeTo: "1.36", EndOfSupport: date(2027, time.May, 31)},
				},
			},
		},
	}
}

// DefaultChannels are the channels this build ships: alpha pinned to the
// shipped release, beta and stable following their upstream.
func DefaultChannels() []*cloudv1alpha1.ReleaseChannel {
	channel := func(name string, spec cloudv1alpha1.ReleaseChannelSpec) *cloudv1alpha1.ReleaseChannel {
		return &cloudv1alpha1.ReleaseChannel{
			TypeMeta:   metav1.TypeMeta{APIVersion: cloudv1alpha1.GroupVersion.String(), Kind: "ReleaseChannel"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       spec,
		}
	}
	return []*cloudv1alpha1.ReleaseChannel{
		channel(cloudv1alpha1.ChannelAlpha, cloudv1alpha1.ReleaseChannelSpec{
			Promotion: cloudv1alpha1.PromotionPinned, Release: DefaultReleaseName}),
		channel(cloudv1alpha1.ChannelBeta, cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionAutomatic}),
		channel(cloudv1alpha1.ChannelStable, cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.PromotionAutomatic}),
	}
}

// CreateReleaseDefaults creates the shipped release and channels that do
// not exist. It never changes an existing object: once the operator has
// edited a channel, or published releases, a new build does not undo it,
// and it never moves alpha to a release by itself.
func CreateReleaseDefaults(ctx context.Context, c client.Client) error {
	channels := DefaultChannels()
	objects := make([]client.Object, 0, len(channels)+1)
	objects = append(objects, DefaultRelease())
	for _, ch := range channels {
		objects = append(objects, ch)
	}
	for _, obj := range objects {
		if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return nil
}

// releaseDefaults runs CreateReleaseDefaults once the manager starts, on
// the leader only.
type releaseDefaults struct {
	c   client.Client
	log func(msg string, args ...any)
}

func (r releaseDefaults) NeedLeaderElection() bool { return true }

func (r releaseDefaults) Start(ctx context.Context) error {
	if err := CreateReleaseDefaults(ctx, r.c); err != nil {
		// Not fatal: the controllers run without a release; the operator
		// can create one.
		r.log("create the shipped release and channels", "error", err)
	}
	return nil
}
