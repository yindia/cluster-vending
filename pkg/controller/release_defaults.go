package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/catalog"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// DefaultReleaseName is the release this build ships: named after its
// content (ShippedReleaseName), since a release's content cannot change. A
// build that ships other versions ships another release.
var DefaultReleaseName = ShippedReleaseName(defaultContent())

const (
	// shippedNotes are the notes of every shipped release.
	shippedNotes = "Shipped with the controller. End-of-support dates are placeholders: publish your own release."
	// legacyShippedName is the release builds shipped before releases were
	// named after their content: the same name with whatever the build
	// pinned, and without the shipped label.
	legacyShippedName = "2026-11-a"
)

// ShippedReleaseName is the name of the release a build ships with
// content: "shipped-" and the first 8 hex digits of the SHA-256 of the
// content's JSON. The same content always gives the same name, and other
// content another one.
func ShippedReleaseName(content cloudv1alpha1.ReleaseContent) string {
	// The JSON of a Go struct is in field order: the same for the same
	// content. Marshalling these types cannot fail.
	raw, _ := json.Marshal(content)
	sum := sha256.Sum256(raw)
	return "shipped-" + hex.EncodeToString(sum[:])[:8]
}

// isShipped reports whether the controller created rel itself: it carries
// the shipped label, or it is the release older builds shipped (its name
// and notes; an operator's release of that name has other notes).
func isShipped(rel *cloudv1alpha1.Release) bool {
	return rel.Labels[cloudv1alpha1.LabelShipped] == "true" ||
		(rel.Name == legacyShippedName && rel.Spec.Notes == shippedNotes)
}

// shippedVersions is the versions this build ships: pkg/catalog/release.yaml, the
// one place they are written. The file is embedded and its tests hold it
// valid, so a failure here is a broken build.
func shippedVersions() catalog.Release {
	rel, err := catalog.ShippedRelease()
	if err != nil {
		panic(err)
	}
	return rel
}

// DefaultRelease is the release this build ships: release.yaml as a
// Release, with this build's renderer version.
func DefaultRelease() *cloudv1alpha1.Release {
	return &cloudv1alpha1.Release{
		TypeMeta: metav1.TypeMeta{APIVersion: cloudv1alpha1.GroupVersion.String(), Kind: "Release"},
		ObjectMeta: metav1.ObjectMeta{Name: DefaultReleaseName,
			Labels: map[string]string{cloudv1alpha1.LabelShipped: "true"}},
		Spec: cloudv1alpha1.ReleaseSpec{Notes: shippedNotes, Content: defaultContent()},
	}
}

// defaultContent is what the shipped release locks.
func defaultContent() cloudv1alpha1.ReleaseContent {
	return ContentOf(shippedVersions())
}

// ContentOf is a release file as the content of a Release: each minor
// pins every component at its version, sorted by name, with the minor's
// own values for the components that have any.
func ContentOf(rel catalog.Release) cloudv1alpha1.ReleaseContent {
	content := cloudv1alpha1.ReleaseContent{RenderVersion: crossplaneaws.RenderVersion,
		Kubernetes: make([]cloudv1alpha1.KubernetesRelease, 0, len(rel.Kubernetes))}
	for _, k := range rel.Kubernetes {
		end, _ := k.EndOfSupportTime() // checked by catalog.ParseRelease
		minor := cloudv1alpha1.KubernetesRelease{Minor: k.Minor, Default: k.Default, Channel: k.Channel, UpgradeTo: k.UpgradeTo,
			EndOfSupport: metav1.NewTime(end)}
		for _, name := range rel.ComponentNames() {
			pin := cloudv1alpha1.ComponentPin{Name: name, Version: rel.Components[name]}
			if values, ok := k.Values[name]; ok {
				raw, _ := json.Marshal(values) // YAML maps of the file: always JSON
				pin.Values = &runtime.RawExtension{Raw: raw}
			}
			minor.Components = append(minor.Components, pin)
		}
		content.Kubernetes = append(content.Kubernetes, minor)
	}
	return content
}

// DefaultChannels are the channels this build ships (release.yaml): a
// Pinned one offers the shipped release, an Automatic one follows the
// channel before it.
func DefaultChannels() []*cloudv1alpha1.ReleaseChannel {
	channels := shippedVersions().Channels
	out := make([]*cloudv1alpha1.ReleaseChannel, 0, len(channels))
	for _, ch := range channels {
		spec := cloudv1alpha1.ReleaseChannelSpec{Promotion: cloudv1alpha1.Promotion(ch.Promotion)}
		if ch.Promotion == catalog.PromotionPinned {
			spec.Release = DefaultReleaseName
		}
		out = append(out, &cloudv1alpha1.ReleaseChannel{
			TypeMeta:   metav1.TypeMeta{APIVersion: cloudv1alpha1.GroupVersion.String(), Kind: "ReleaseChannel"},
			ObjectMeta: metav1.ObjectMeta{Name: ch.Name},
			Spec:       spec,
		})
	}
	return out
}

// CreateReleaseDefaults creates this build's shipped release and the
// channels that do not exist, and moves alpha to the shipped release while
// alpha is still as a build left it (takeOverAlpha).
//
// It never changes anything of the operator's: a release they published,
// a channel they edited, and the releases older builds shipped all stay
// (clusters may still run them). beta and stable are never written: as
// Automatic channels they follow alpha by themselves.
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
	return takeOverAlpha(ctx, c)
}

// asShipped reports whether a channel's settings are what a build gives
// alpha, whatever release it names: Pinned, not paused, and a rollout
// policy nobody set (empty, or the defaults the API server fills in).
func asShipped(spec cloudv1alpha1.ReleaseChannelSpec) bool {
	// The counts are defaulted by the API server (5 and 1). The soak is
	// not: the controller sends its zero value, which is stored as "0s"
	// instead of the 24h default, so both count as nobody's choice.
	filledIn := cloudv1alpha1.RolloutPolicy{MaxInProgress: 5, MaxFailed: 1, Soak: metav1.Duration{Duration: 24 * time.Hour}}
	asStored := cloudv1alpha1.RolloutPolicy{MaxInProgress: 5, MaxFailed: 1}
	policy := equality.Semantic.DeepEqual(spec.Policy, cloudv1alpha1.RolloutPolicy{}) ||
		equality.Semantic.DeepEqual(spec.Policy, filledIn) || equality.Semantic.DeepEqual(spec.Policy, asStored)
	return spec.Promotion == cloudv1alpha1.PromotionPinned && !spec.Paused && policy
}

// takeOverAlpha points alpha at this build's shipped release when alpha is
// exactly what a build made it: Pinned to a shipped release (isShipped),
// with nothing else set. Then no operator has taken alpha over, and the
// new build's versions must reach the channels. Anything else is the
// operator's and stays: another promotion, a pause, a rollout policy, a
// release they published, or a release that no longer exists.
func takeOverAlpha(ctx context.Context, c client.Client) error {
	alpha := &cloudv1alpha1.ReleaseChannel{}
	if err := c.Get(ctx, client.ObjectKey{Name: cloudv1alpha1.ChannelAlpha}, alpha); err != nil {
		return client.IgnoreNotFound(err)
	}
	current := alpha.Spec.Release
	if current == "" || current == DefaultReleaseName || !asShipped(alpha.Spec) {
		return nil
	}
	rel := &cloudv1alpha1.Release{}
	if err := c.Get(ctx, client.ObjectKey{Name: current}, rel); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !isShipped(rel) {
		return nil
	}
	alpha.Spec.Release = DefaultReleaseName
	if err := c.Update(ctx, alpha); err != nil {
		return fmt.Errorf("move channel alpha from the shipped release %s to %s: %w", current, DefaultReleaseName, err)
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
