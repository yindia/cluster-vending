package aws

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// What an EKS token is made of (aws-iam-authenticator's token format, the
// one `aws eks get-token` prints): a presigned sts:GetCallerIdentity URL
// that names the cluster in a signed header, base64url-encoded behind a
// prefix. EKS calls the URL to learn who presents it.
const (
	eksTokenPrefix = "k8s-aws-v1."
	// eksClusterHeader is the signed header that binds a token to one
	// cluster: a token minted for one cluster is refused by another.
	eksClusterHeader = "x-k8s-aws-id"
	// eksTokenExpiresHeader and eksTokenExpiry bound how long the presigned
	// URL can be used. EKS accepts a token for 15 minutes at most.
	eksTokenExpiresHeader = "X-Amz-Expires"
	eksTokenExpiry        = 60
)

// KubernetesAccess is how the Kubernetes API of an EKS cluster is reached
// as the tenant role: where it is, the certificate authority that signs
// its certificate, and a bearer token. The token is short-lived (EKS
// accepts it for 15 minutes) and is not to be stored or logged.
type KubernetesAccess struct {
	Endpoint string
	// CA is the PEM certificate authority of the endpoint.
	CA    []byte
	Token string
}

// ClusterAPIAccess reaches into a tenant's cluster. The tenant role is the
// identity the platform created the cluster with (its ProviderConfig), so
// EKS knows it as a cluster admin; a cluster that was imported knows it
// from its takeover on.
type ClusterAPIAccess interface {
	// EKSKubernetesAccess returns the access to the Kubernetes API of the
	// EKS cluster named name. ErrNotFound when EKS has no such cluster.
	EKSKubernetesAccess(ctx context.Context, access Access, region, name string) (KubernetesAccess, error)
}

var _ ClusterAPIAccess = (*Client)(nil)

// STSPresignAPI is the part of the STS presign client an EKS token needs.
type STSPresignAPI interface {
	PresignGetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// EKSKubernetesAccess is Tenant.KubernetesAccess after VerifyAccess.
func (c *Client) EKSKubernetesAccess(ctx context.Context, access Access, region, name string) (KubernetesAccess, error) {
	t, err := c.verified(ctx, access, region)
	if err != nil {
		return KubernetesAccess{}, err
	}
	return t.KubernetesAccess(ctx, name)
}

// KubernetesAccess describes the EKS cluster for its endpoint and
// certificate authority and mints a token for it as the tenant role. It
// changes nothing in the account.
//
// Calls: eks:DescribeCluster. Minting the token calls nothing (the URL is
// signed locally); EKS calls sts:GetCallerIdentity with it, which needs no
// permission.
func (t *Tenant) KubernetesAccess(ctx context.Context, name string) (KubernetesAccess, error) {
	out, err := t.eks.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: awssdk.String(name)})
	if err != nil {
		return KubernetesAccess{}, translateEKS(err, "describe EKS cluster "+name)
	}
	endpoint := awssdk.ToString(out.Cluster.Endpoint)
	if endpoint == "" || out.Cluster.CertificateAuthority == nil {
		return KubernetesAccess{}, fmt.Errorf("%w: EKS cluster %s has no Kubernetes API endpoint yet", ErrNotFound, name)
	}
	ca, err := base64.StdEncoding.DecodeString(awssdk.ToString(out.Cluster.CertificateAuthority.Data))
	if err != nil {
		return KubernetesAccess{}, fmt.Errorf("EKS cluster %s: its certificate authority is not base64: %w", name, err)
	}
	token, err := t.eksToken(ctx, name)
	if err != nil {
		return KubernetesAccess{}, err
	}
	return KubernetesAccess{Endpoint: endpoint, CA: ca, Token: token}, nil
}

// eksToken mints a token for the EKS cluster named name as the tenant
// role.
func (t *Tenant) eksToken(ctx context.Context, name string) (string, error) {
	if t.presign == nil {
		return "", fmt.Errorf("%w: no STS presign client to mint a token for EKS cluster %s", ErrInvalid, name)
	}
	bound := func(o *sts.Options) {
		o.APIOptions = append(o.APIOptions,
			smithyhttp.AddHeaderValue(eksClusterHeader, name),
			smithyhttp.AddHeaderValue(eksTokenExpiresHeader, strconv.Itoa(eksTokenExpiry)))
	}
	req, err := t.presign.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(o *sts.PresignOptions) {
		o.ClientOptions = append(o.ClientOptions, bound)
	})
	if err != nil {
		return "", translate(err, "mint a token for EKS cluster "+name)
	}
	return eksTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(req.URL)), nil
}
