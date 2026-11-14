package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/cloudformation"
)

const demoRole = "cluster-vending-demo-0115-provisioner"

// A role name of its own (cluster-vending-<anything>-<role>) gets that
// role's template, as a stack named after it, the RoleName parameter
// set, and the tag account delete --delete-role looks for.
func TestAccountCreateWithARoleOfItsOwn(t *testing.T) {
	api := &accountAPI{verdict: verified()}
	stacks := &fakeStacks{caller: tenantAccount, states: []*StackInfo{nil, stack("CREATE_COMPLETE")}}

	r := createAccount(t, Options{Input: manifest(t, demoRole), CreateRole: "terminal"}, api, stacks)

	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.progress)
	}
	if api.onboarded.GetRole() != "provisioner" || api.onboarded.GetRoleName() != demoRole {
		t.Errorf("onboarding asked = %v", api.onboarded)
	}
	if len(stacks.created) != 1 || stacks.created[0].Name != demoRole || stacks.created[0].RoleName != demoRole ||
		stacks.created[0].Tags[cloudformation.ManagedByTag] != cloudformation.Platform {
		t.Errorf("created = %+v", stacks.created)
	}
}

// deleteAccount runs account delete with o, against api and, as the
// caller's AWS credentials, stacks (nil: there are none).
func deleteAccount(t *testing.T, o Options, api *accountAPI, stacks *fakeStacks) run {
	t.Helper()
	r := run{api: api, stacks: stacks}
	var out, progress bytes.Buffer
	o.Progress, o.PollInterval, o.ID, o.Tenant = &progress, time.Nanosecond, accountID, "acme"
	if o.Timeout == 0 {
		o.Timeout = DefaultWaitTimeout
	}
	o.RoleStacks = func(context.Context, string, string) (RoleStacks, error) {
		if stacks == nil {
			return nil, errors.New("no AWS credentials")
		}
		return stacks, nil
	}
	r.err = Run(t.Context(), AccountDelete, Clients{Tenant: api}, o, &out, OutputTable)
	r.out, r.progress = out.String(), progress.String()
	return r
}

func ours(status string) *StackInfo {
	s := stack(status)
	s.Tags = map[string]string{cloudformation.ManagedByTag: cloudformation.Platform}
	return s
}

func TestAccountDelete(t *testing.T) {
	tests := []struct {
		name      string
		o         Options
		role      string
		stacks    *fakeStacks
		deleteErr error
		// wantDeleted: the account; wantStack: the stack deleted.
		wantDeleted, wantStack bool
		wantErr, wantProgress  string
	}{
		{name: "the account only; the role stays", o: Options{Wait: true}, role: demoRole, wantDeleted: true,
			wantProgress: "deleted"},
		{name: "and the stack the CLI created", o: Options{Wait: true, DeleteRole: true}, role: demoRole,
			stacks:      &fakeStacks{caller: tenantAccount, states: []*StackInfo{ours("CREATE_COMPLETE"), ours("DELETE_IN_PROGRESS"), nil}},
			wantDeleted: true, wantStack: true, wantProgress: "Stack " + demoRole + ": deleted"},
		{name: "a stack the CLI did not create is refused, and nothing is deleted", o: Options{Wait: true, DeleteRole: true},
			role:    "cluster-vending-provisioner",
			stacks:  &fakeStacks{caller: tenantAccount, states: []*StackInfo{stack("CREATE_COMPLETE")}},
			wantErr: "was not created by cluster-vending"},
		{name: "a role no stack holds is refused, and nothing is deleted", o: Options{Wait: true, DeleteRole: true},
			role: demoRole, stacks: &fakeStacks{caller: tenantAccount, states: []*StackInfo{nil}},
			wantErr: "no stack " + demoRole},
		{name: "a role none of the templates creates is refused", o: Options{DeleteRole: true}, role: "acme-admin",
			stacks: &fakeStacks{caller: tenantAccount}, wantErr: "not a role the platform's templates create"},
		{name: "credentials of another account: nothing is deleted", o: Options{DeleteRole: true}, role: demoRole,
			stacks: &fakeStacks{caller: platformAccount}, wantErr: "your AWS credentials are of account " + platformAccount},
		{name: "networks keep the account, and its stack", o: Options{Wait: true, DeleteRole: true}, role: demoRole,
			stacks:    &fakeStacks{caller: tenantAccount, states: []*StackInfo{ours("CREATE_COMPLETE")}},
			deleteErr: connect.NewError(connect.CodeFailedPrecondition, "networks prod belong to the account"),
			wantErr:   "networks prod"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &accountAPI{role: tt.role, gets: 2, deleteErr: tt.deleteErr}

			r := deleteAccount(t, tt.o, api, tt.stacks)

			if tt.wantErr != "" {
				if r.err == nil || !bytes.Contains([]byte(r.err.Error()), []byte(tt.wantErr)) {
					t.Fatalf("err = %v, want %q\n%s", r.err, tt.wantErr, r.progress)
				}
			} else if r.err != nil {
				t.Fatalf("%v\n%s", r.err, r.progress)
			}
			if api.deleted != tt.wantDeleted {
				t.Errorf("account deleted = %v, want %v", api.deleted, tt.wantDeleted)
			}
			if got := tt.stacks != nil && len(tt.stacks.deleted) == 1 && tt.stacks.deleted[0] == tt.role; got != tt.wantStack {
				t.Errorf("stack deleted = %v (%v), want %v", got, tt.stacks, tt.wantStack)
			}
			if tt.wantProgress != "" {
				contains(t, "progress", r.progress, tt.wantProgress)
			}
			if tt.wantDeleted && tt.wantErr == "" {
				contains(t, "output", r.out, "cloudaccount/acme-prod ("+accountID+") deleted")
			}
		})
	}
}
