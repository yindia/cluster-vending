package aws

import (
	"context"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// tagsFake answers the CreateTags check: DescribeVpcs lists vpcs, and
// CreateTags answers answer for the resource it was asked to tag.
type tagsFake struct {
	probeFake
	vpcs   []string
	answer func(resource string) error
	tagged *[]string
}

func (f tagsFake) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	out := &ec2.DescribeVpcsOutput{}
	for _, id := range f.vpcs {
		out.Vpcs = append(out.Vpcs, ec2types.Vpc{VpcId: awssdk.String(id)})
	}
	return out, nil
}

func (f tagsFake) CreateVpc(_ context.Context, in *ec2.CreateVpcInput, _ ...func(*ec2.Options)) (*ec2.CreateVpcOutput, error) {
	if !awssdk.ToBool(in.DryRun) {
		panic("CreateVpc without DryRun: the check must never create anything")
	}
	if len(in.TagSpecifications) != 1 || in.TagSpecifications[0].ResourceType != ec2types.ResourceTypeVpc || len(in.TagSpecifications[0].Tags) == 0 {
		panic("the tag check's CreateVpc must tag the VPC on create")
	}
	*f.tagged = append(*f.tagged, "create-vpc")
	return nil, f.answer("create-vpc")
}

func (f tagsFake) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	if !awssdk.ToBool(in.DryRun) {
		panic("CreateTags without DryRun: the check must never change anything")
	}
	*f.tagged = append(*f.tagged, in.Resources...)
	return nil, f.answer(in.Resources[0])
}

// The dry-run tag is made on a VPC the role can see, so the answer is the
// permission's and never the resource's: AWS rejects a made-up ID such as
// vpc-00000000000000000 (InvalidID) before it checks the permission, so no
// made-up ID is ever used. In a region with no VPC the permission is tested
// by tagging on create (a dry-run CreateVpc with the tag), which AWS
// authorizes as ec2:CreateTags on the new VPC.
func TestCreateTagsCheckUsesAnExistingVPC(t *testing.T) {
	tests := []struct {
		name   string
		vpcs   []string
		answer func(string) error
		target string
		ok     bool
	}{
		{name: "allowed on an existing vpc", vpcs: []string{"vpc-0a1b2c3d4e5f60718"},
			answer: func(string) error { return apiErr("DryRunOperation", "Request would have succeeded") },
			target: "vpc-0a1b2c3d4e5f60718", ok: true},
		{name: "denied on an existing vpc", vpcs: []string{"vpc-0a1b2c3d4e5f60718"},
			answer: func(string) error { return apiErr("UnauthorizedOperation", "You are not authorized") },
			target: "vpc-0a1b2c3d4e5f60718"},
		{name: "a not-found answer on an existing vpc is not a pass", vpcs: []string{"vpc-0a1b2c3d4e5f60718"},
			answer: func(string) error { return apiErr("InvalidVpcID.NotFound", "does not exist") },
			target: "vpc-0a1b2c3d4e5f60718"},
		{name: "no vpc in the region: tag on create allowed",
			answer: func(string) error { return apiErr("DryRunOperation", "Request would have succeeded") },
			target: "create-vpc", ok: true},
		{name: "no vpc in the region: tag on create refused",
			answer: func(string) error { return apiErr("UnauthorizedOperation", "You are not authorized") },
			target: "create-vpc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls, tagged []string
			f := tagsFake{probeFake: probeFake{calls: &calls}, vpcs: tt.vpcs, answer: tt.answer, tagged: &tagged}

			err := call(t.Context(), accessAPIs{ec2: f}, ActionCreateTagsDryRun)

			if (err == nil) != tt.ok {
				t.Errorf("err = %v, want ok=%v", err, tt.ok)
			}
			if len(tagged) != 1 || tagged[0] != tt.target {
				t.Errorf("tagged %v, want [%s]", tagged, tt.target)
			}
		})
	}
}
