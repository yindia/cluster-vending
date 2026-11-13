package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
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

// ClusterKubeconfig gives the caller access to the cluster's Kubernetes
// API from the cluster ID alone: it reads the EKS name and the region from
// the server and runs `aws eks update-kubeconfig` with them, which adds
// the cluster to the caller's kubeconfig (IAM, with their own AWS
// credentials).
//
// --print prints that kubeconfig instead of installing it; --oidc prints
// one whose user signs in through the cluster's OIDC identity provider
// (spec.access.oidc) with kubelogin (kubectl oidc-login). A printed file
// has the cluster's recorded endpoint and CA, and no credential: its user
// is an exec entry.
func ClusterKubeconfig(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.GetClusterRequest{TenantId: o.Tenant, ClusterId: o.ID}, c.Cluster.GetCluster)
	if err != nil {
		return nil, err
	}
	cluster := res.(*cloudv1.GetClusterResponse).GetCluster()
	if !o.OIDC && !o.Print {
		return nil, updateKubeconfig(ctx, cluster, o)
	}
	build := IAMKubeconfig
	if o.OIDC {
		build = OIDCKubeconfig
	}
	raw, err := build(cluster)
	if err != nil {
		return nil, err
	}
	_, err = o.Out.Write(raw)
	return nil, err
}

// updateKubeconfig runs `aws eks update-kubeconfig` for cluster, with the
// EKS name and the region the server recorded: the AWS CLI adds the
// cluster to the caller's kubeconfig (o.KubeconfigFile, or its default)
// and makes it the current context.
func updateKubeconfig(ctx context.Context, cluster *cloudv1.Cluster, o Options) error {
	name, region, err := eksNameAndRegion(cluster)
	if err != nil {
		return err
	}
	args := []string{"eks", "update-kubeconfig", "--name", name, "--region", region}
	if o.KubeconfigFile != "" {
		args = append(args, "--kubeconfig", o.KubeconfigFile)
	}
	run := o.Exec
	if run == nil {
		run = runLocal
	}
	if err := run(ctx, o.Out, "aws", args...); err != nil {
		return fmt.Errorf("aws %s: %w (is the AWS CLI installed and signed in to the cluster's account? --print prints the kubeconfig instead)",
			strings.Join(args, " "), err)
	}
	return nil
}

// runLocal runs a command of the caller's machine, attached to their
// terminal (the AWS CLI may ask for an MFA code).
func runLocal(ctx context.Context, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // "aws" with a name and a region checked by eksNameAndRegion; no shell
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, w, os.Stderr
	return cmd.Run()
}

// eksNameAndRegion are what the AWS CLI needs to find the cluster: the EKS
// name in the cluster's ARN (the platform's name before the ARN is known)
// and the cluster's region.
func eksNameAndRegion(cluster *cloudv1.Cluster) (name, region string, err error) {
	name, region = cluster.GetName(), cluster.GetSpec().GetRegion()
	if region == "" {
		return "", "", fmt.Errorf("cluster %s has no region", name)
	}
	if arn := cluster.GetStatus().GetAws().GetClusterArn(); arn != "" {
		if _, after, ok := strings.Cut(arn, ":cluster/"); ok && after != "" {
			name = after
		}
	}
	// They become arguments of a local command: only what an EKS name and
	// an AWS region can be is passed on, whatever the server answered.
	if !eksNamePattern.MatchString(name) {
		return "", "", fmt.Errorf("cluster name %q is not an EKS cluster name", name)
	}
	if !regionPattern.MatchString(region) {
		return "", "", fmt.Errorf("region %q is not an AWS region", region)
	}
	return name, region, nil
}

var (
	eksNamePattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_-]{0,99}$`)
	regionPattern  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
)

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
	eksName, region, err := eksNameAndRegion(cluster)
	if err != nil {
		return nil, err
	}
	return kubeconfigWith(cluster, cluster.GetName()+"-iam", &clientcmdapi.ExecConfig{
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
