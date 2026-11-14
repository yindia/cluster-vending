package cluster

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A new cluster gets IRSA unless it asks for none; asking for none is
// refused while platform components use it (they would never install).
func TestIRSADefault(t *testing.T) {
	users := []string{"aws-load-balancer-controller", "karpenter"}
	tests := []struct {
		name    string
		irsa    *bool
		users   []string
		want    bool
		wantErr string
	}{
		{name: "unset: on", irsa: nil, users: users, want: true},
		{name: "true stays true", irsa: proto.Bool(true), users: users, want: true},
		{name: "false while components use it: refused", irsa: proto.Bool(false), users: users,
			wantErr: "aws-load-balancer-controller, karpenter"},
		{name: "false when nothing uses it: kept", irsa: proto.Bool(false), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &cloudv1.ClusterSpec{Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{Irsa: tt.irsa}}}

			err := defaultIRSA(spec, tt.users)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !fault.Is(err, fault.Invalid) {
					t.Fatalf("err = %v, want an Invalid fault naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || spec.GetAws().GetIrsa() != tt.want || spec.GetAws().Irsa == nil {
				t.Fatalf("irsa = %v (%v), want %v set", spec.GetAws().Irsa, err, tt.want)
			}
		})
	}
}

// Without an AWS section there is nothing to default.
func TestIRSADefaultWithoutAWS(t *testing.T) {
	if err := defaultIRSA(&cloudv1.ClusterSpec{}, []string{"karpenter"}); err != nil {
		t.Fatal(err)
	}
}
