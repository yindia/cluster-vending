package cli

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/oidc"
)

const (
	// execAPIVersion is the exec credential API kubectl speaks.
	execAPIVersion = "client.authentication.k8s.io/v1"
	// kubeloginHint is shown by kubectl when the plugin is not installed.
	kubeloginHint = "kubectl oidc-login is not installed: kubectl krew install oidc-login " +
		"(or brew install kubelogin); see https://github.com/int128/kubelogin"
)

// ClusterKubeconfig prints a kubeconfig for the cluster's Kubernetes API
// whose user signs in through the cluster's OIDC identity provider
// (spec.access.oidc): --oidc. The endpoint and the CA are the cluster's
// recorded status; the user is an exec entry for kubelogin
// (kubectl oidc-login), which opens the provider's sign-in page and keeps
// the token on the user's machine. The file carries no credential.
func ClusterKubeconfig(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if !o.OIDC {
		return nil, errors.New("only an OIDC kubeconfig is printed (--oidc); " +
			"for IAM access run: aws eks update-kubeconfig --name <cluster name> --region <region>")
	}
	res, err := call(ctx, &cloudv1.GetClusterRequest{TenantId: o.Tenant, ClusterId: o.ID}, c.Cluster.GetCluster)
	if err != nil {
		return nil, err
	}
	raw, err := OIDCKubeconfig(res.(*cloudv1.GetClusterResponse).GetCluster())
	if err != nil {
		return nil, err
	}
	_, err = o.Out.Write(raw)
	return nil, err
}

// OIDCKubeconfig is the kubeconfig of cluster for users of its OIDC
// identity provider.
func OIDCKubeconfig(cluster *cloudv1.Cluster) ([]byte, error) {
	name := cluster.GetName()
	provider := oidc.Resolve(cluster.GetSpec().GetAccess().GetOidc())
	if provider == nil {
		return nil, fmt.Errorf("cluster %s has no OIDC identity provider (spec.access.oidc)", name)
	}
	endpoint := cluster.GetStatus().GetEndpoint()
	if endpoint.GetUrl() == "" {
		return nil, fmt.Errorf("cluster %s has no API endpoint yet (%s)", name, cluster.GetStatus().GetState())
	}
	if len(endpoint.GetCertificateAuthorityData()) == 0 {
		return nil, fmt.Errorf("cluster %s has no certificate authority recorded yet", name)
	}
	args := []string{"oidc-login", "get-token", "--oidc-issuer-url=" + provider.IssuerURL, "--oidc-client-id=" + provider.ClientID}
	for _, scope := range provider.Scopes() {
		args = append(args, "--oidc-extra-scope="+scope)
	}
	user := name + "-oidc"
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[name] = &clientcmdapi.Cluster{Server: endpoint.GetUrl(), CertificateAuthorityData: endpoint.GetCertificateAuthorityData()}
	cfg.AuthInfos[user] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{
		APIVersion: execAPIVersion, Command: "kubectl", Args: args,
		InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode, InstallHint: kubeloginHint,
	}}
	cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: user}
	cfg.CurrentContext = name
	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig of cluster %s: %w", name, err)
	}
	return raw, nil
}
