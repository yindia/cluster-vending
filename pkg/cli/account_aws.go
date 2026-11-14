package cli

import (
	"context"
	"errors"
	"maps"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
)

// awsRoleStacks are the account's stacks through the AWS SDK's default
// credential chain (environment, shared config and SSO, the profile
// named), in region.
func awsRoleStacks(ctx context.Context, profile, region string) (RoleStacks, error) {
	opts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return sdkStacks{cf: cfn.NewFromConfig(cfg), sts: sts.NewFromConfig(cfg)}, nil
}

type sdkStacks struct {
	cf  *cfn.Client
	sts *sts.Client
}

func (s sdkStacks) CallerAccount(ctx context.Context) (string, error) {
	out, err := s.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Account), nil
}

func (s sdkStacks) Stack(ctx context.Context, name string) (*StackInfo, error) {
	out, err := s.cf.DescribeStacks(ctx, &cfn.DescribeStacksInput{StackName: aws.String(name)})
	if isAPIError(err, "ValidationError", "does not exist") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(out.Stacks) == 0 {
		return nil, nil
	}
	st := out.Stacks[0]
	info := &StackInfo{Status: string(st.StackStatus), Reason: aws.ToString(st.StackStatusReason), Tags: map[string]string{}}
	for _, p := range st.Parameters {
		switch aws.ToString(p.ParameterKey) {
		case cloudformation.ParameterManagementRoleARN:
			info.ManagementRoleARN = aws.ToString(p.ParameterValue)
		case cloudformation.ParameterRoleName:
			info.RoleName = aws.ToString(p.ParameterValue)
		}
	}
	for _, t := range st.Tags {
		info.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return info, nil
}

func (s sdkStacks) TemplateBody(ctx context.Context, name string) (string, error) {
	out, err := s.cf.GetTemplate(ctx, &cfn.GetTemplateInput{StackName: aws.String(name), TemplateStage: cfntypes.TemplateStageOriginal})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.TemplateBody), nil
}

func (s sdkStacks) CreateStack(ctx context.Context, in StackInput) error {
	_, err := s.cf.CreateStack(ctx, &cfn.CreateStackInput{StackName: aws.String(in.Name), TemplateBody: aws.String(in.Body),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityNamedIam}, Parameters: stackParameters(in),
		Tags: stackTags(in)})
	return err
}

func (s sdkStacks) DeleteStack(ctx context.Context, name string) error {
	_, err := s.cf.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: aws.String(name)})
	return err
}

// stackTags are in's tags, in name order (a stable request).
func stackTags(in StackInput) []cfntypes.Tag {
	keys := slices.Sorted(maps.Keys(in.Tags))
	out := make([]cfntypes.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, cfntypes.Tag{Key: aws.String(k), Value: aws.String(in.Tags[k])})
	}
	return out
}

func (s sdkStacks) UpdateStack(ctx context.Context, in StackInput) (bool, error) {
	_, err := s.cf.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: aws.String(in.Name), TemplateBody: aws.String(in.Body),
		Capabilities: []cfntypes.Capability{cfntypes.CapabilityCapabilityNamedIam}, Parameters: stackParameters(in)})
	if isAPIError(err, "ValidationError", "No updates are to be performed") {
		return false, nil
	}
	return err == nil, err
}

func (s sdkStacks) StackEvents(ctx context.Context, name string) ([]StackEvent, error) {
	out, err := s.cf.DescribeStackEvents(ctx, &cfn.DescribeStackEventsInput{StackName: aws.String(name)})
	if err != nil {
		return nil, err
	}
	events := make([]StackEvent, 0, len(out.StackEvents))
	for _, e := range out.StackEvents {
		events = append(events, StackEvent{ID: aws.ToString(e.EventId), Resource: aws.ToString(e.LogicalResourceId),
			Type: aws.ToString(e.ResourceType), Status: string(e.ResourceStatus), Reason: aws.ToString(e.ResourceStatusReason)})
	}
	return events, nil
}

func stackParameters(in StackInput) []cfntypes.Parameter {
	params := []cfntypes.Parameter{{ParameterKey: aws.String(cloudformation.ParameterManagementRoleARN),
		ParameterValue: aws.String(in.ManagementRoleARN)}}
	if in.RoleName != "" {
		params = append(params, cfntypes.Parameter{ParameterKey: aws.String(cloudformation.ParameterRoleName),
			ParameterValue: aws.String(in.RoleName)})
	}
	return params
}

// isAPIError reports whether err is the AWS API error code whose message
// contains text.
func isAPIError(err error, code, text string) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == code && strings.Contains(api.ErrorMessage(), text)
}

// openURL opens url in the default browser, without waiting for it.
func openURL(ctx context.Context, url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url) //nolint:gosec // the console link the API server built
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url) //nolint:gosec // as above
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", url) //nolint:gosec // as above
	}
	return cmd.Start()
}
