package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestPrincipalFromCaller(t *testing.T) {
	for _, tt := range []struct {
		arn, want, role, err string
	}{
		{arn: "arn:aws:iam::111122223333:user/alice", want: "arn:aws:iam::111122223333:user/alice"},
		{arn: "arn:aws:iam::111122223333:user/ops/alice", want: "arn:aws:iam::111122223333:user/ops/alice"},
		{arn: "arn:aws:sts::111122223333:assumed-role/admin/alice@example.com", want: "arn:aws:iam::111122223333:role/admin", role: "admin"},
		{arn: "arn:aws-us-gov:sts::111122223333:assumed-role/AWSReservedSSO_Admin_0123/alice", want: "arn:aws-us-gov:iam::111122223333:role/AWSReservedSSO_Admin_0123", role: "AWSReservedSSO_Admin_0123"},
		{arn: "arn:aws:iam::111122223333:root", err: "cannot be a cluster admin"},
		{arn: "arn:aws:sts::111122223333:federated-user/bob", err: "cannot be a cluster admin"},
		{arn: "not-an-arn", err: "not an IAM ARN"},
	} {
		t.Run(tt.arn, func(t *testing.T) {
			got, role, err := principalFromCaller(tt.arn)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want || role != tt.role {
				t.Fatalf("= %q, %q, %v; want %q, %q", got, role, err, tt.want, tt.role)
			}
		})
	}
}

// createRecorder records CreateCluster.
type createRecorder struct {
	cloudv1connect.ClusterServiceClient
	created *cloudv1.CreateClusterRequest
}

func (r *createRecorder) CreateCluster(_ context.Context, req *cloudv1.CreateClusterRequest) (*cloudv1.CreateClusterResponse, error) {
	r.created = req
	return &cloudv1.CreateClusterResponse{Cluster: req.GetCluster()}, nil
}

const createManifest = `apiVersion: cluster-vending.io/v1
kind: Cluster
metadata: {name: prod-eks, tenant: acme}
spec:
  region: us-west-1
  network: {networkId: 01a1dfca-f5dd-7144-97b5-baeefe0e4a39}
  controlPlane: {kubernetesVersion: "1.35", endpointAccess: {publicAccessCidrs: ["0.0.0.0/0"]}}
  access:
    adminPrincipals: [arn:aws:iam::111122223333:role/ops]
  aws: {}
`

func TestClusterCreateAddsTheCaller(t *testing.T) {
	const me = "arn:aws:iam::111122223333:role/admin"
	found := func(context.Context, string) (string, string, error) { return me, "", nil }
	for _, tt := range []struct {
		name     string
		manifest string
		opts     Options
		want     []string
		progress string
	}{
		{"added", createManifest, Options{CallerPrincipal: found},
			[]string{"arn:aws:iam::111122223333:role/ops", me}, "Adding you (" + me + ")"},
		{"already there", strings.Replace(createManifest, "role/ops", "role/admin", 1), Options{CallerPrincipal: found},
			[]string{me}, ""},
		{"--no-self-admin", createManifest, Options{NoSelfAdmin: true, CallerPrincipal: found},
			[]string{"arn:aws:iam::111122223333:role/ops"}, ""},
		{"no credentials", createManifest, Options{CallerPrincipal: func(context.Context, string) (string, string, error) {
			return "", "", errNoCaller
		}}, []string{"arn:aws:iam::111122223333:role/ops"}, "no human admin"},
		{"role path unknown", createManifest, Options{CallerPrincipal: func(context.Context, string) (string, string, error) {
			return me, "the path of role admin is unknown", nil
		}}, []string{"arn:aws:iam::111122223333:role/ops", me}, "Note: the path of role admin is unknown"},
		{"already there, first", createManifest, Options{CallerPrincipal: func(context.Context, string) (string, string, error) {
			return "arn:aws:iam::111122223333:role/ops", "", nil
		}}, []string{"arn:aws:iam::111122223333:role/ops"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &createRecorder{}
			var out, progress bytes.Buffer
			o := tt.opts
			o.Input, o.Progress = writeManifest(t, tt.manifest), &progress
			if err := Run(t.Context(), ClusterCreate, Clients{Cluster: api}, o, &out, OutputJSON); err != nil {
				t.Fatal(err)
			}
			if got := api.created.GetCluster().GetSpec().GetAccess().GetAdminPrincipals(); !slices.Equal(got, tt.want) {
				t.Errorf("adminPrincipals = %v, want %v", got, tt.want)
			}
			if tt.progress != "" && !strings.Contains(progress.String(), tt.progress) {
				t.Errorf("progress %q lacks %q", progress.String(), tt.progress)
			}
		})
	}
}

func TestClusterCreateSkipsAnAccessEntryPrincipal(t *testing.T) {
	c := &cloudv1.Cluster{Spec: &cloudv1.ClusterSpec{Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}},
		Access: &cloudv1.AccessSpec{AccessEntries: []*cloudv1.AccessEntry{{PrincipalArn: "arn:aws:iam::111122223333:role/dev"}}}}}
	var progress bytes.Buffer
	addSelfAdmin(t.Context(), Options{Progress: &progress, CallerPrincipal: func(context.Context, string) (string, string, error) {
		return "arn:aws:iam::111122223333:role/dev", "", nil
	}}, c)
	if len(c.GetSpec().GetAccess().GetAdminPrincipals()) != 0 || !strings.Contains(progress.String(), "accessEntries") {
		t.Fatalf("admins %v, progress %q", c.GetSpec().GetAccess().GetAdminPrincipals(), progress.String())
	}
	cm := &cloudv1.Cluster{Spec: &cloudv1.ClusterSpec{Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{
		AccessConfig: &cloudv1.AwsAccessConfig{AuthenticationMode: cloudv1.AwsAuthenticationMode_AWS_AUTHENTICATION_MODE_CONFIG_MAP}}}}}
	addSelfAdmin(t.Context(), Options{Progress: &progress, CallerPrincipal: func(context.Context, string) (string, string, error) {
		return "", "", errors.New("must not be asked")
	}}, cm)
	if cm.GetSpec().GetAccess() != nil {
		t.Fatal("CONFIG_MAP cluster got an admin")
	}
}
