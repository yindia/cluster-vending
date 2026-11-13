package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

const (
	kubeconfigCluster = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"
	kubeconfigCA      = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	kubeconfigTenant  = "833ced3d-cb2e-41de-92f1-29e2af035ddc"
	kubeconfigClient  = "00001111-aaaa-2222-bbbb-3333cccc4444"
)

// oneCluster answers GetCluster with cluster.
type oneCluster struct {
	cloudv1connect.ClusterServiceClient
	cluster *cloudv1.Cluster
	err     error
	asked   []*cloudv1.GetClusterRequest
}

func (s *oneCluster) GetCluster(_ context.Context, req *cloudv1.GetClusterRequest) (*cloudv1.GetClusterResponse, error) {
	s.asked = append(s.asked, req)
	if s.err != nil {
		return nil, s.err
	}
	return &cloudv1.GetClusterResponse{Cluster: s.cluster}, nil
}

// runningCluster is a cluster with an endpoint and the OIDC provider p.
func runningCluster(p *cloudv1.OidcIdentityProvider) *cloudv1.Cluster {
	return &cloudv1.Cluster{Id: kubeconfigCluster, Name: "prod-eks",
		Spec: &cloudv1.ClusterSpec{Access: &cloudv1.AccessSpec{Oidc: p}},
		Status: &cloudv1.ClusterStatus{Endpoint: &cloudv1.ClusterEndpoint{
			Url: "https://ABC.gr7.us-west-1.eks.amazonaws.com", CertificateAuthorityData: []byte(kubeconfigCA)}}}
}

func TestClusterKubeconfigOIDC(t *testing.T) {
	tests := []struct {
		name     string
		provider *cloudv1.OidcIdentityProvider
		wantArgs []string
	}{
		{
			name:     "entra",
			provider: &cloudv1.OidcIdentityProvider{Entra: &cloudv1.EntraIdentityProvider{TenantId: kubeconfigTenant, ClientId: kubeconfigClient}},
			wantArgs: []string{"oidc-login", "get-token",
				"--oidc-issuer-url=https://login.microsoftonline.com/" + kubeconfigTenant + "/v2.0",
				"--oidc-client-id=" + kubeconfigClient,
				"--oidc-extra-scope=profile", "--oidc-extra-scope=offline_access"},
		},
		{
			name: "a generic provider",
			provider: &cloudv1.OidcIdentityProvider{Name: "okta", IssuerUrl: "https://example.okta.com/oauth2/default",
				ClientId: "kubernetes", UsernameClaim: "email", GroupsClaim: "groups"},
			wantArgs: []string{"oidc-login", "get-token", "--oidc-issuer-url=https://example.okta.com/oauth2/default",
				"--oidc-client-id=kubernetes", "--oidc-extra-scope=email", "--oidc-extra-scope=groups"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &oneCluster{cluster: runningCluster(tt.provider)}
			var out bytes.Buffer

			err := Run(t.Context(), ClusterKubeconfig, Clients{Cluster: svc},
				Options{Tenant: "acme", ID: kubeconfigCluster, OIDC: true}, &out, OutputYAML)

			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(svc.asked) != 1 || svc.asked[0].GetTenantId() != "acme" || svc.asked[0].GetClusterId() != kubeconfigCluster {
				t.Errorf("asked %v", svc.asked)
			}
			// What kubectl reads from it.
			cfg, err := clientcmd.Load(out.Bytes())
			if err != nil {
				t.Fatalf("not a kubeconfig: %v\n%s", err, out.String())
			}
			cluster, user, kctx := cfg.Clusters["prod-eks"], cfg.AuthInfos["prod-eks-oidc"], cfg.Contexts["prod-eks"]
			if cluster == nil || user == nil || kctx == nil || cfg.CurrentContext != "prod-eks" {
				t.Fatalf("kubeconfig = %s", out.String())
			}
			if cluster.Server != "https://ABC.gr7.us-west-1.eks.amazonaws.com" || string(cluster.CertificateAuthorityData) != kubeconfigCA ||
				kctx.Cluster != "prod-eks" || kctx.AuthInfo != "prod-eks-oidc" {
				t.Errorf("cluster %+v, context %+v", cluster, kctx)
			}
			exec := user.Exec
			if exec == nil || exec.Command != "kubectl" || exec.APIVersion != "client.authentication.k8s.io/v1" ||
				!slices.Equal(exec.Args, tt.wantArgs) || exec.InteractiveMode != "IfAvailable" || !strings.Contains(exec.InstallHint, "oidc-login") {
				t.Errorf("exec = %+v, want args %v", exec, tt.wantArgs)
			}
			// Nothing secret: no token, no client secret, no certificate key.
			if user.Token != "" || len(user.ClientKeyData) > 0 || user.Password != "" || strings.Contains(out.String(), "secret") {
				t.Errorf("the kubeconfig carries a credential:\n%s", out.String())
			}
		})
	}
}

func TestClusterKubeconfigFailures(t *testing.T) {
	entra := &cloudv1.OidcIdentityProvider{Entra: &cloudv1.EntraIdentityProvider{TenantId: kubeconfigTenant, ClientId: kubeconfigClient}}
	noEndpoint := runningCluster(entra)
	noEndpoint.Status.Endpoint = nil
	noCA := runningCluster(entra)
	noCA.Status.Endpoint.CertificateAuthorityData = nil
	tests := []struct {
		name string
		svc  *oneCluster
		opts Options
		want string
	}{
		{"no tenant", &oneCluster{cluster: runningCluster(entra)}, Options{ID: kubeconfigCluster, OIDC: true}, "--tenant"},
		{"no provider", &oneCluster{cluster: runningCluster(nil)}, Options{Tenant: "acme", ID: kubeconfigCluster, OIDC: true}, "spec.access.oidc"},
		{"no endpoint yet", &oneCluster{cluster: noEndpoint}, Options{Tenant: "acme", ID: kubeconfigCluster, OIDC: true}, "endpoint"},
		{"no CA yet", &oneCluster{cluster: noCA}, Options{Tenant: "acme", ID: kubeconfigCluster, OIDC: true}, "certificate authority"},
		{"the API fails", &oneCluster{err: errors.New("boom")}, Options{Tenant: "acme", ID: kubeconfigCluster, OIDC: true}, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			err := Run(t.Context(), ClusterKubeconfig, Clients{Cluster: tt.svc}, tt.opts, &out, OutputYAML)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run = %v, want an error naming %q", err, tt.want)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q with the error", out.String())
			}
		})
	}
}

// Without --oidc the kubeconfig signs in with IAM: the EKS name and the
// region come from the cluster, so its ID is all the caller gives.
func TestClusterKubeconfigIAM(t *testing.T) {
	cluster := runningCluster(nil)
	cluster.Spec.Region = "us-west-1"
	cluster.Status.Provider = &cloudv1.ClusterStatus_Aws{Aws: &cloudv1.AwsClusterStatus{ClusterArn: "arn:aws:eks:us-west-1:111122223333:cluster/prod-eks-live"}}
	var out bytes.Buffer

	err := Run(t.Context(), ClusterKubeconfig, Clients{Cluster: &oneCluster{cluster: cluster}},
		Options{Tenant: "acme", ID: kubeconfigCluster}, &out, OutputYAML)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cfg, err := clientcmd.Load(out.Bytes())
	if err != nil {
		t.Fatalf("not a kubeconfig: %v\n%s", err, out.String())
	}
	exec := cfg.AuthInfos[cfg.Contexts[cfg.CurrentContext].AuthInfo].Exec
	want := []string{"eks", "get-token", "--cluster-name", "prod-eks-live", "--region", "us-west-1", "--output", "json"}
	if exec == nil || exec.Command != "aws" || !slices.Equal(exec.Args, want) {
		t.Fatalf("exec = %+v, want aws %v", exec, want)
	}
	if cfg.Clusters[cfg.Contexts[cfg.CurrentContext].Cluster].Server != cluster.Status.Endpoint.Url {
		t.Errorf("server = %v", cfg.Clusters)
	}
}

// Before the EKS cluster reports its ARN the platform's cluster name is
// the EKS name.
func TestClusterKubeconfigIAMBeforeTheARN(t *testing.T) {
	cluster := runningCluster(nil)
	cluster.Spec.Region = "us-west-1"

	raw, err := IAMKubeconfig(cluster)

	if err != nil || !strings.Contains(string(raw), "prod-eks") {
		t.Fatalf("IAMKubeconfig = %s, %v", raw, err)
	}
}
