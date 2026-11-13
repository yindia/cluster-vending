package cli

import (
	"context"
	"fmt"
	"strings"

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
	// awsCLIHint is shown by kubectl when the AWS CLI is not installed.
	awsCLIHint = "the AWS CLI is not installed: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"
)

// ClusterKubeconfig prints a kubeconfig for the cluster's Kubernetes API.
// The endpoint and the CA are the cluster's recorded status, and the file
// carries no credential: its user is an exec entry. By default that is
// `aws eks get-token` (IAM, with the caller's own AWS credentials); with
// --oidc it is kubelogin (kubectl oidc-login) for the cluster's OIDC
// identity provider (spec.access.oidc), which opens the provider's
// sign-in page and keeps the token on the user's machine.
func ClusterKubeconfig(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.GetClusterRequest{TenantId: o.Tenant, ClusterId: o.ID}, c.Cluster.GetCluster)
	if err != nil {
		return nil, err
	}
	build := IAMKubeconfig
	if o.OIDC {
		build = OIDCKubeconfig
	}
	raw, err := build(res.(*cloudv1.GetClusterResponse).GetCluster())
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
	args := []string{"oidc-login", "get-token", "--oidc-issuer-url=" + provider.IssuerURL, "--oidc-client-id=" + provider.ClientID}
	for _, scope := range provider.Scopes() {
		args = append(args, "--oidc-extra-scope="+scope)
	}
	return kubeconfigWith(cluster, name+"-oidc", &clientcmdapi.ExecConfig{
		APIVersion: execAPIVersion, Command: "kubectl", Args: args,
		InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode, InstallHint: kubeloginHint,
	})
}

// IAMKubeconfig is the kubeconfig of cluster for IAM principals: its user
// runs `aws eks get-token` with the caller's own AWS credentials. The EKS
// name is the one in the cluster's ARN (the platform's name before the ARN
// is known) and the region is the cluster's.
func IAMKubeconfig(cluster *cloudv1.Cluster) ([]byte, error) {
	name := cluster.GetName()
	region := cluster.GetSpec().GetRegion()
	if region == "" {
		return nil, fmt.Errorf("cluster %s has no region", name)
	}
	eksName := name
	if arn := cluster.GetStatus().GetAws().GetClusterArn(); arn != "" {
		if _, after, ok := strings.Cut(arn, ":cluster/"); ok && after != "" {
			eksName = after
		}
	}
	return kubeconfigWith(cluster, name+"-iam", &clientcmdapi.ExecConfig{
		APIVersion: execAPIVersion, Command: "aws",
		Args:            []string{"eks", "get-token", "--cluster-name", eksName, "--region", region, "--output", "json"},
		InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode, InstallHint: awsCLIHint,
	})
}

// kubeconfigWith is a kubeconfig of cluster's recorded endpoint and CA
// whose one user signs in with exec.
func kubeconfigWith(cluster *cloudv1.Cluster, user string, exec *clientcmdapi.ExecConfig) ([]byte, error) {
	name := cluster.GetName()
	endpoint := cluster.GetStatus().GetEndpoint()
	if endpoint.GetUrl() == "" {
		return nil, fmt.Errorf("cluster %s has no API endpoint yet (%s)", name, cluster.GetStatus().GetState())
	}
	if len(endpoint.GetCertificateAuthorityData()) == 0 {
		return nil, fmt.Errorf("cluster %s has no certificate authority recorded yet", name)
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[name] = &clientcmdapi.Cluster{Server: endpoint.GetUrl(), CertificateAuthorityData: endpoint.GetCertificateAuthorityData()}
	cfg.AuthInfos[user] = &clientcmdapi.AuthInfo{Exec: exec}
	cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: user}
	cfg.CurrentContext = name
	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig of cluster %s: %w", name, err)
	}
	return raw, nil
}
