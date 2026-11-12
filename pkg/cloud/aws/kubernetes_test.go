package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// describedEKS answers DescribeCluster with cluster; the rest is fakeEKS.
type describedEKS struct {
	fakeEKS
	cluster *ekstypes.Cluster
}

func (f describedEKS) DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	return &eks.DescribeClusterOutput{Cluster: f.cluster}, nil
}

// presigner signs with fixed credentials, like the tenant's STS client: no
// call is made.
func presigner() STSPresignAPI {
	return sts.NewPresignClient(sts.NewFromConfig(awssdk.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIAEXAMPLE", "secret", "session"),
	}))
}

const testCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

func activeCluster() *ekstypes.Cluster {
	return &ekstypes.Cluster{
		Endpoint: awssdk.String("https://ABCDEF.gr7.us-east-1.eks.amazonaws.com"),
		CertificateAuthority: &ekstypes.Certificate{
			Data: awssdk.String(base64.StdEncoding.EncodeToString([]byte(testCA))),
		},
	}
}

func TestKubernetesAccessMintsATokenBoundToTheCluster(t *testing.T) {
	// Arrange
	tenant := &Tenant{AccountID: tenantAccount, Region: "us-east-1",
		eks: describedEKS{cluster: activeCluster()}, presign: presigner()}

	// Act
	got, err := tenant.KubernetesAccess(t.Context(), "prod")

	// Assert
	if err != nil {
		t.Fatalf("KubernetesAccess: %v", err)
	}
	if got.Endpoint != "https://ABCDEF.gr7.us-east-1.eks.amazonaws.com" || string(got.CA) != testCA {
		t.Errorf("endpoint %q, CA %q", got.Endpoint, got.CA)
	}
	encoded, ok := strings.CutPrefix(got.Token, "k8s-aws-v1.")
	if !ok {
		t.Fatalf("token %q: no k8s-aws-v1. prefix", got.Token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("token is not unpadded base64url: %v", err)
	}
	signed, err := url.Parse(string(raw))
	if err != nil {
		t.Fatalf("token does not hold a URL: %v", err)
	}
	q := signed.Query()
	if signed.Host != "sts.us-east-1.amazonaws.com" || q.Get("Action") != "GetCallerIdentity" {
		t.Errorf("presigned %s %s: want sts:GetCallerIdentity at the regional endpoint", signed.Host, q.Get("Action"))
	}
	// The cluster name is a signed header: EKS refuses the token of
	// another cluster.
	if headers := q.Get("X-Amz-SignedHeaders"); !strings.Contains(headers, "x-k8s-aws-id") {
		t.Errorf("signed headers %q: the cluster name is not signed", headers)
	}
	if q.Get("X-Amz-Expires") != "60" {
		t.Errorf("X-Amz-Expires %q, want 60", q.Get("X-Amz-Expires"))
	}
	if q.Get("X-Amz-Security-Token") != "session" {
		t.Error("the token is not signed with the tenant role's session credentials")
	}
}

func TestKubernetesAccessFailures(t *testing.T) {
	tests := []struct {
		name string
		eks  EKSAPI
		sign STSPresignAPI
		want error
	}{
		{name: "EKS has no such cluster",
			eks: fakeEKS{describeErr: &ekstypes.ResourceNotFoundException{}}, sign: presigner(), want: ErrNotFound},
		{name: "the cluster has no endpoint yet",
			eks: describedEKS{cluster: &ekstypes.Cluster{}}, sign: presigner(), want: ErrNotFound},
		{name: "no presign client",
			eks: describedEKS{cluster: activeCluster()}, want: ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant := &Tenant{AccountID: tenantAccount, Region: "us-east-1", eks: tt.eks, presign: tt.sign}

			got, err := tenant.KubernetesAccess(t.Context(), "prod")

			if !errors.Is(err, tt.want) {
				t.Errorf("error %v, want %v", err, tt.want)
			}
			if got.Token != "" {
				t.Error("a token was returned with an error")
			}
		})
	}
}

func TestKubernetesAccessRefusesACertificateAuthorityThatIsNotBase64(t *testing.T) {
	cluster := activeCluster()
	cluster.CertificateAuthority.Data = awssdk.String("not base64!")
	tenant := &Tenant{AccountID: tenantAccount, Region: "us-east-1", eks: describedEKS{cluster: cluster}, presign: presigner()}

	_, err := tenant.KubernetesAccess(t.Context(), "prod")

	if err == nil || !strings.Contains(err.Error(), "certificate authority") {
		t.Errorf("error %v, want one about the certificate authority", err)
	}
}
