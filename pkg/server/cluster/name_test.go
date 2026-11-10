package cluster

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2"
	"go.uber.org/mock/gomock"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository/mock"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// fakeLookup answers like EKS: default Kubernetes 1.34, add-on versions
// "v1-<k8s>" (the recommended one) and "v0-<k8s>".
type fakeLookup struct{ exists bool }

func (f fakeLookup) EKSClusterExists(context.Context, awscloud.Access, string, string) (bool, error) {
	return f.exists, nil
}

func (f fakeLookup) ResolveEKSVersions(_ context.Context, _ awscloud.Access, _ string, req awscloud.VersionRequest) (awscloud.VersionResult, error) {
	k8s := req.KubernetesVersion
	if k8s == "" {
		k8s = "1.34"
	}
	out := awscloud.VersionResult{KubernetesVersion: k8s, Addons: map[string]awscloud.AddonVersion{}}
	for _, a := range req.Addons {
		switch a.Version {
		case "":
			out.Addons[a.Name] = awscloud.AddonVersion{Version: "v1-" + k8s}
		case "v1-" + k8s, "v0-" + k8s:
			out.Addons[a.Name] = awscloud.AddonVersion{Version: a.Version}
		default:
			return awscloud.VersionResult{}, fmt.Errorf("%w: %s %s not compatible with %s", awscloud.ErrInvalid, a.Name, a.Version, k8s)
		}
	}
	return out, nil
}

func TestCheckName(t *testing.T) {
	account := &model.CloudAccount{Base: model.Base{ID: "acct-1"}, AccountID: "111122223333"}
	tgt := target{account: account}
	same := model.Cluster{Base: model.Base{ID: "c-1"}, Name: "prod", Region: "us-east-1", NetworkID: "n-1"}
	other := same
	other.Region = "eu-west-1"
	tests := []struct {
		name      string
		existing  []model.Cluster
		awsExists bool
		want      connect.Code
	}{
		{"free", nil, false, 0},
		{"platform cluster in same account and region", []model.Cluster{same}, false, connect.CodeAlreadyExists},
		{"platform cluster in another region", []model.Cluster{other}, false, 0},
		{"cluster made outside the platform", nil, true, connect.CodeAlreadyExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store, clusters, networks := mock.NewMockStore(ctrl), mock.NewMockClusters(ctrl), mock.NewMockNetworks(ctrl)
			store.EXPECT().Clusters().Return(clusters).AnyTimes()
			store.EXPECT().Networks().Return(networks).AnyTimes()
			clusters.EXPECT().ListByName(gomock.Any(), "acme", "prod").Return(tt.existing, nil)
			networks.EXPECT().Get(gomock.Any(), "acme", "n-1").
				Return(&model.Network{CloudAccountID: account.ID}, nil).AnyTimes()
			s := NewClusterService(service.Deps{Store: store, AWSClusters: fakeLookup{tt.awsExists},
				AWS: config.AWS{StaticKeys: &config.StaticKeys{AccessKeyID: "AKIA", SecretAccessKey: "x"}}})
			err := s.checkName(context.Background(), "acme", "prod", "us-east-1", tgt)
			if got := connect.CodeOf(err); (tt.want == 0 && err != nil) || (tt.want != 0 && got != tt.want) {
				t.Errorf("err = %v, want code %v", err, tt.want)
			}
		})
	}
}
