package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// upstreamOf is the channel an Automatic channel follows.
var upstreamOf = map[string]string{cloudv1alpha1.ChannelBeta: cloudv1alpha1.ChannelAlpha,
	cloudv1alpha1.ChannelStable: cloudv1alpha1.ChannelBeta}

// PromoteReleaseChannel asks an Automatic channel to take a release now,
// before its release soak ends (cloudv1alpha1.AnnotationPromote): the
// upstream channel's release, or the one named. The controller takes it
// on its next pass and records the promotion in the channel's history.
func (s *Service) PromoteReleaseChannel(ctx context.Context, req *cloudv1.PromoteReleaseChannelRequest) (*cloudv1.PromoteReleaseChannelResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	ch, err := s.channel(ctx, req.GetChannel())
	if err != nil {
		return nil, err
	}
	if ch.Spec.Promotion != "" && ch.Spec.Promotion != cloudv1alpha1.PromotionAutomatic {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"channel %s is %s to %s: it takes no promotion; set its spec.release", ch.Name, ch.Spec.Promotion, ch.Spec.Release))
	}
	release := req.GetRelease()
	if release == "" {
		upper := &cloudv1alpha1.ReleaseChannel{}
		if err := s.reader().Get(ctx, client.ObjectKey{Name: upstreamOf[ch.Name]}, upper); err != nil {
			return nil, s.readError(ctx, "release channel "+upstreamOf[ch.Name], err)
		}
		if release = upper.Status.Release; release == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Sprintf("channel %s offers no release yet: name one with --release", upstreamOf[ch.Name]))
		}
	}
	if err := s.promotable(ctx, release); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cloudv1alpha1.ChannelPromotion{Release: release, By: caller(ctx), At: metav1.NewTime(s.now().UTC())})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	before := client.MergeFrom(ch.DeepCopy())
	if ch.Annotations == nil {
		ch.Annotations = map[string]string{}
	}
	ch.Annotations[cloudv1alpha1.AnnotationPromote] = string(raw)
	if err := s.deps.Kube.Patch(ctx, ch, before); err != nil {
		s.logger().ErrorContext(ctx, "promote release channel", "channel", ch.Name, "release", release, "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "cannot write the promotion to the management cluster; try again")
	}
	note := fmt.Sprintf("%s takes %s on the controller's next pass", ch.Name, release)
	if ch.Status.Release == release {
		note = fmt.Sprintf("%s offers %s already", ch.Name, release)
	}
	return &cloudv1.PromoteReleaseChannelResponse{Channel: ChannelFromAPI(ch, true), Note: note}, nil
}

// promotable refuses a release a channel cannot offer: missing, not valid
// or withdrawn.
func (s *Service) promotable(ctx context.Context, name string) error {
	rel := &cloudv1alpha1.Release{}
	err := s.reader().Get(ctx, client.ObjectKey{Name: name}, rel)
	switch {
	case apierrors.IsNotFound(err):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("release %s does not exist", name))
	case err != nil:
		return s.readError(ctx, "release "+name, err)
	case rel.Spec.Withdrawn:
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("release %s is withdrawn", name))
	case !meta.IsStatusConditionTrue(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid):
		why := "not checked yet"
		if c := meta.FindStatusCondition(rel.Status.Conditions, cloudv1alpha1.ReleaseConditionValid); c != nil {
			why = c.Message
		}
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("release %s is not valid: %s", name, why))
	}
	return nil
}
