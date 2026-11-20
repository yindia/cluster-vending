package aws

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// fakeProfiles is an account's instance profiles holding one role.
type fakeProfiles struct {
	tags    map[string]map[string]string // by profile name
	holding map[string]bool              // profiles that hold the role
	ops     []string
	failTag string // profile whose tags cannot be read
	listErr error
}

func (f *fakeProfiles) ListInstanceProfilesForRole(_ context.Context, in *iam.ListInstanceProfilesForRoleInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfilesForRoleOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := &iam.ListInstanceProfilesForRoleOutput{}
	for name := range f.tags {
		if f.holding[name] {
			n := name
			out.InstanceProfiles = append(out.InstanceProfiles, types.InstanceProfile{InstanceProfileName: &n})
		}
	}
	slices.SortFunc(out.InstanceProfiles, func(a, b types.InstanceProfile) int {
		return compare(*a.InstanceProfileName, *b.InstanceProfileName)
	})
	return out, nil
}

func compare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func (f *fakeProfiles) ListInstanceProfileTags(_ context.Context, in *iam.ListInstanceProfileTagsInput, _ ...func(*iam.Options)) (*iam.ListInstanceProfileTagsOutput, error) {
	name := *in.InstanceProfileName
	if name == f.failTag {
		return nil, errors.New("throttled")
	}
	out := &iam.ListInstanceProfileTagsOutput{}
	for k, v := range f.tags[name] {
		out.Tags = append(out.Tags, types.Tag{Key: &k, Value: &v})
	}
	return out, nil
}

func (f *fakeProfiles) RemoveRoleFromInstanceProfile(_ context.Context, in *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	f.ops = append(f.ops, "remove "+*in.InstanceProfileName)
	delete(f.holding, *in.InstanceProfileName)
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

func (f *fakeProfiles) DeleteInstanceProfile(_ context.Context, in *iam.DeleteInstanceProfileInput, _ ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	f.ops = append(f.ops, "delete "+*in.InstanceProfileName)
	delete(f.tags, *in.InstanceProfileName)
	return &iam.DeleteInstanceProfileOutput{}, nil
}

func newFakeProfiles() *fakeProfiles {
	return &fakeProfiles{
		tags: map[string]map[string]string{
			"karp-ours":   {TagKarpenterNodeClass: "default", TagClusterPrefix + "prod": "owned", TagEKSClusterName: "prod"},
			"karp-by-tag": {TagKarpenterManagedBy: "prod"},
			"karp-other":  {TagKarpenterNodeClass: "default", TagClusterPrefix + "staging": "owned", TagEKSClusterName: "staging"},
			"by-hand":     {TagClusterPrefix + "prod": "owned"},
			"untagged":    {},
		},
		holding: map[string]bool{"karp-ours": true, "karp-by-tag": true, "karp-other": true, "by-hand": true, "untagged": true},
	}
}

// Only the profiles Karpenter made for this cluster lose the role and are
// deleted, the role first; another cluster's Karpenter profiles and profiles
// made by hand are never touched.
func TestReleaseKarpenterRoleTouchesOnlyThisClustersKarpenterProfiles(t *testing.T) {
	f := newFakeProfiles()

	deleted, err := ReleaseKarpenterRole(t.Context(), f, "prod-karpenter-node", "prod")

	if err != nil {
		t.Fatal(err)
	}
	want := []string{"remove karp-by-tag", "delete karp-by-tag", "remove karp-ours", "delete karp-ours"}
	if !slices.Equal(f.ops, want) {
		t.Errorf("operations %v, want %v", f.ops, want)
	}
	if !slices.Equal(deleted, []string{"karp-by-tag", "karp-ours"}) {
		t.Errorf("deleted %v", deleted)
	}
	for _, kept := range []string{"karp-other", "by-hand", "untagged"} {
		if !f.holding[kept] {
			t.Errorf("profile %s lost the role, it is not this cluster's Karpenter's", kept)
		}
	}
}

// A profile whose tags cannot be read is left alone and reported; the
// others are still done.
func TestReleaseKarpenterRoleReportsWhatItCannotRead(t *testing.T) {
	f := newFakeProfiles()
	f.failTag = "karp-by-tag"

	deleted, err := ReleaseKarpenterRole(t.Context(), f, "prod-karpenter-node", "prod")

	if err == nil || !slices.Equal(deleted, []string{"karp-ours"}) {
		t.Fatalf("deleted %v, err %v", deleted, err)
	}
	if !f.holding["karp-by-tag"] {
		t.Error("a profile whose tags are unknown was changed")
	}
}

// A role that is gone holds nothing; and no cluster name matches nothing.
func TestReleaseKarpenterRoleEdges(t *testing.T) {
	f := newFakeProfiles()
	f.listErr = &types.NoSuchEntityException{}
	if deleted, err := ReleaseKarpenterRole(t.Context(), f, "gone", "prod"); err != nil || len(deleted) != 0 {
		t.Errorf("a role that is gone: %v, %v", deleted, err)
	}
	f = newFakeProfiles()
	if deleted, err := ReleaseKarpenterRole(t.Context(), f, "prod-karpenter-node", ""); err != nil || len(deleted) != 0 || len(f.ops) != 0 {
		t.Errorf("an empty cluster name matched: %v, %v, %v", deleted, err, f.ops)
	}
}
