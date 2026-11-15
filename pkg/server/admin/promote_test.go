package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// An admin promotes an Automatic channel: it is asked to take the release
// now (the controller acts on it), with who asked. A release that is not
// valid, and alpha, are refused.
func TestPromoteReleaseChannel(t *testing.T) {
	beta := cloudv1.ReleaseChannel_RELEASE_CHANNEL_BETA

	tests := []struct {
		name    string
		req     *cloudv1.PromoteReleaseChannelRequest
		code    connect.Code
		want    string // the release asked for
		message string // a fragment of the error
	}{
		{name: "the upstream's release", req: &cloudv1.PromoteReleaseChannelRequest{Channel: beta}, want: shipped},
		{name: "a named release", req: &cloudv1.PromoteReleaseChannelRequest{Channel: beta, Release: shipped}, want: shipped},
		{name: "a release that is not valid", req: &cloudv1.PromoteReleaseChannelRequest{Channel: beta, Release: "shipped-f874cd2c"},
			code: connect.CodeFailedPrecondition, message: "not valid"},
		{name: "a release that does not exist", req: &cloudv1.PromoteReleaseChannelRequest{Channel: beta, Release: "2030-01-a"},
			code: connect.CodeFailedPrecondition, message: "does not exist"},
		{name: "alpha", req: &cloudv1.PromoteReleaseChannelRequest{Channel: cloudv1.ReleaseChannel_RELEASE_CHANNEL_ALPHA},
			code: connect.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := releaseWorld(t)

			res, err := s.PromoteReleaseChannel(t.Context(), tt.req)

			if tt.code != 0 {
				if connect.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.message) {
					t.Fatalf("err = %v, want %v naming %q", err, tt.code, tt.message)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ch := &cloudv1alpha1.ReleaseChannel{}
			if err := s.deps.Kube.Get(t.Context(), client.ObjectKey{Name: "beta"}, ch); err != nil {
				t.Fatal(err)
			}
			p := cloudv1alpha1.ChannelPromotion{}
			if err := json.Unmarshal([]byte(ch.Annotations[cloudv1alpha1.AnnotationPromote]), &p); err != nil || p.Release != tt.want || p.By == "" {
				t.Errorf("promotion = %+v (%v), want %s with who asked", p, err, tt.want)
			}
			if !strings.Contains(res.GetNote(), "beta") || !strings.Contains(res.GetNote(), tt.want) {
				t.Errorf("note = %q", res.GetNote())
			}
		})
	}
}
