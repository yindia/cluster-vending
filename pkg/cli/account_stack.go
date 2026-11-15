package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// RoleStacks is the tenant account's CloudFormation and STS as the
// caller's AWS credentials reach them: what account create needs to
// create the platform's role there.
type RoleStacks interface {
	// CallerAccount is the account the credentials are of.
	CallerAccount(ctx context.Context) (string, error)
	// Stack is the stack named name; nil when there is none.
	Stack(ctx context.Context, name string) (*StackInfo, error)
	// TemplateBody is the template the stack was created or last updated
	// with.
	TemplateBody(ctx context.Context, name string) (string, error)
	CreateStack(ctx context.Context, in StackInput) error
	// UpdateStack reports false when CloudFormation finds nothing to
	// update.
	UpdateStack(ctx context.Context, in StackInput) (bool, error)
	// StackEvents are the stack's latest events, newest first.
	StackEvents(ctx context.Context, name string) ([]StackEvent, error)
	DeleteStack(ctx context.Context, name string) error
}

// StackInfo is a stack as DescribeStacks reports it.
type StackInfo struct {
	Status, Reason string
	// ManagementRoleARN and RoleName are the stack's parameters.
	ManagementRoleARN, RoleName string
	Tags                        map[string]string
}

// createdByCLI reports whether account create made the stack: it carries
// cloudformation.ManagedByTag (or the managed-by tag of earlier builds).
func (s *StackInfo) createdByCLI() bool {
	return s.Tags[cloudformation.ManagedByTag] == cloudformation.Platform || s.Tags["managed-by"] == cloudformation.Platform
}

// StackInput is the stack of one role: its name, template, the management
// role the role trusts, the role's name, and the stack's tags.
type StackInput struct {
	Name, Body, ManagementRoleARN, RoleName string
	// ExternalID is the account's (the template's ExternalId parameter);
	// empty: none.
	ExternalID string
	Tags       map[string]string
}

// StackEvent is one event of a stack.
type StackEvent struct {
	ID, Resource, Type, Status, Reason string
}

// defaultPollInterval is how often account create looks at the stack and
// at the account's access check.
const defaultPollInterval = 5 * time.Second

func (o Options) pollInterval() time.Duration {
	if o.PollInterval <= 0 {
		return defaultPollInterval
	}
	return o.PollInterval
}

// stackSucceeded and stackFailed are the stack statuses a wait ends on;
// any other status is still in progress.
func stackSucceeded(status string) bool {
	return status == "CREATE_COMPLETE" || status == "UPDATE_COMPLETE"
}

func stackFailed(status string) bool {
	return strings.HasSuffix(status, "_FAILED") || strings.HasSuffix(status, "ROLLBACK_COMPLETE") ||
		strings.HasPrefix(status, "DELETE_") || status == "IMPORT_ROLLBACK_COMPLETE"
}

// ensureStack creates the role's stack, updates it when its template or
// management role differ, or leaves it as it is; then follows it to the
// end, printing its events.
func ensureStack(ctx context.Context, o Options, s RoleStacks, ob *cloudv1.AccountOnboarding, account string) error {
	in := StackInput{Name: ob.GetStackName(), Body: ob.GetTemplateBody(), ManagementRoleARN: ob.GetManagementRoleArn(),
		RoleName: ob.GetRoleName(), ExternalID: ob.GetExternalId(), Tags: map[string]string{cloudformation.ManagedByTag: cloudformation.Platform}}
	if in.ExternalID != "" {
		// CloudFormation shows a NoEcho parameter masked: the tag says
		// which external ID the stack has, without it.
		in.Tags[cloudformation.ExternalIDTag] = cloudformation.ExternalIDFingerprint(in.ExternalID)
	}
	info, err := s.Stack(ctx, in.Name)
	if err != nil {
		return fmt.Errorf("read the stack %s in account %s: %w", in.Name, account, err)
	}
	seen, err := eventIDs(ctx, s, in.Name, info)
	if err != nil {
		return err
	}
	switch {
	case info == nil:
		o.progressf("Creating the stack %s in account %s (%s): the IAM role %s, trusting %s\n",
			in.Name, account, ob.GetRegion(), ob.GetRoleName(), in.ManagementRoleARN)
		if err := s.CreateStack(ctx, in); err != nil {
			return fmt.Errorf("create the stack %s: %w", in.Name, err)
		}
	case stackSucceeded(info.Status):
		same, err := sameStack(ctx, s, in, info)
		if err != nil {
			return err
		}
		if same {
			o.progressf("The stack %s already creates the role %s: nothing to do\n", in.Name, ob.GetRoleName())
			return nil
		}
		o.progressf("Updating the stack %s in account %s: its template, management role or external ID changed\n", in.Name, account)
		changed, err := s.UpdateStack(ctx, in)
		if err != nil {
			return fmt.Errorf("update the stack %s: %w", in.Name, err)
		}
		if !changed {
			o.progressf("The stack %s has nothing to update\n", in.Name)
			return nil
		}
	case stackFailed(info.Status):
		return fmt.Errorf("the stack %s in account %s is %s: delete it in the account's CloudFormation console, then run account create again",
			in.Name, account, info.Status)
	default:
		o.progressf("The stack %s is %s: waiting for it\n", in.Name, info.Status)
	}
	return followStack(ctx, o, s, in.Name, seen)
}

// sameStack reports whether the stack has the role's template and
// management role.
func sameStack(ctx context.Context, s RoleStacks, in StackInput, info *StackInfo) (bool, error) {
	// A stack of a template before RoleName has no such parameter: its
	// template differs, which the comparison below finds.
	if info.ManagementRoleARN != in.ManagementRoleARN || (info.RoleName != "" && info.RoleName != in.RoleName) {
		return false, nil
	}
	if in.ExternalID != "" && info.Tags[cloudformation.ExternalIDTag] != in.Tags[cloudformation.ExternalIDTag] {
		return false, nil // made without the account's external ID (or with another)
	}
	body, err := s.TemplateBody(ctx, in.Name)
	if err != nil {
		return false, fmt.Errorf("read the template of the stack %s: %w", in.Name, err)
	}
	return strings.TrimSpace(body) == strings.TrimSpace(in.Body), nil
}

// eventIDs are the events the stack has already: followStack shows only
// the ones after them.
func eventIDs(ctx context.Context, s RoleStacks, name string, info *StackInfo) (map[string]bool, error) {
	seen := map[string]bool{}
	if info == nil {
		return seen, nil
	}
	events, err := s.StackEvents(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("read the events of the stack %s: %w", name, err)
	}
	for _, e := range events {
		seen[e.ID] = true
	}
	return seen, nil
}

// followStack waits until the stack succeeds or fails, printing each new
// event; a failure returns the reasons of the events that failed.
func followStack(ctx context.Context, o Options, s RoleStacks, name string, seen map[string]bool) error {
	var failures []string
	show := func() error {
		events, err := s.StackEvents(ctx, name)
		if err != nil {
			return fmt.Errorf("read the events of the stack %s: %w", name, err)
		}
		for i := len(events) - 1; i >= 0; i-- { // oldest first
			e := events[i]
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			line := fmt.Sprintf("  %s   %s   %s", e.Resource, e.Type, e.Status)
			if e.Reason != "" {
				line += "   " + e.Reason
			}
			o.progressf("%s\n", line)
			if strings.HasSuffix(e.Status, "_FAILED") && e.Reason != "" {
				failures = append(failures, e.Resource+": "+e.Reason)
			}
		}
		return nil
	}
	return poll(ctx, o, "the stack "+name, func() (bool, error) {
		if err := show(); err != nil {
			return false, err
		}
		info, err := s.Stack(ctx, name)
		if err != nil {
			return false, fmt.Errorf("read the stack %s: %w", name, err)
		}
		switch {
		case info == nil:
			return false, fmt.Errorf("the stack %s is gone", name)
		case stackSucceeded(info.Status):
			if err := show(); err != nil {
				return false, err
			}
			o.progressf("Stack %s: %s\n", name, info.Status)
			return true, nil
		case stackFailed(info.Status):
			if err := show(); err != nil {
				return false, err
			}
			why := strings.Join(failures, "; ")
			if why == "" {
				why = info.Reason
			}
			return false, fmt.Errorf("the stack %s failed (%s): %s; the account was not onboarded", name, info.Status, why)
		}
		return false, nil
	})
}

// awaitStack waits until a stack someone else creates (in the console)
// exists, then follows it.
func awaitStack(ctx context.Context, o Options, s RoleStacks, name string) error {
	o.progressf("Waiting for the stack %s to be created in the console\n", name)
	err := poll(ctx, o, "the stack "+name, func() (bool, error) {
		info, err := s.Stack(ctx, name)
		if err != nil {
			return false, fmt.Errorf("read the stack %s: %w", name, err)
		}
		return info != nil, nil
	})
	if err != nil {
		return err
	}
	return followStack(ctx, o, s, name, map[string]bool{})
}

// poll calls done every poll interval until it reports true or fails,
// for at most --timeout.
func poll(ctx context.Context, o Options, what string, done func() (bool, error)) error {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	for {
		ok, err := done()
		if err != nil || ok {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for %s after %s (--timeout)", what, o.Timeout)
		case <-time.After(o.pollInterval()):
		}
	}
}

// progressf writes to Progress (stderr), when there is one.
func (o Options) progressf(format string, args ...any) {
	if o.Progress != nil {
		_, _ = fmt.Fprintf(o.Progress, format, args...) // progress on stderr: nothing to do if it cannot be written
	}
}
