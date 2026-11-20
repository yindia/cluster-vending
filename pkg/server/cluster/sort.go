package cluster

import (
	"cmp"
	"slices"
	"strconv"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// defaultSortKey is the number of the first, default key of every sort enum.
const defaultSortKey = 1

// sortScope names the sort of a list for its page tokens: "" for the kind's
// default sort and order (so the tokens of a request that does not sort stay
// what they were), else a string that a token made with another sort does
// not match. by is the enum's number: 0 (unspecified) and 1 (the first key) are
// the default key of every kind; desc is the effective direction.
func sortScope(by int32, desc, defaultDesc bool) string {
	if by == 0 {
		by = defaultSortKey
	}
	if by == defaultSortKey && desc == defaultDesc {
		return ""
	}
	return "\x00sort=" + strconv.Itoa(int(by)) + "," + strconv.FormatBool(desc)
}

// isDesc is the direction a request asks for: its order, or the key's
// default when unspecified.
func isDesc(order cloudv1.SortOrder, defaultDesc bool) bool {
	switch order {
	case cloudv1.SortOrder_SORT_ORDER_ASC:
		return false
	case cloudv1.SortOrder_SORT_ORDER_DESC:
		return true
	default:
		return defaultDesc
	}
}

// directed makes a comparison descending when desc; equal stays equal, so a
// stable sort keeps the list's own order among ties.
func directed[T any](desc bool, f func(a, b T) int) func(a, b T) int {
	if !desc {
		return f
	}
	return func(a, b T) int { return -f(a, b) }
}

// offsetOf is the start of the page a token names. Without a sort scope the
// token is a plain offset (base is a prefix of the binding the list is
// served with, or "" for the Cluster object's copy); with one it is bound to
// the scope as well as to the object.
func pageStart(binding, scope, token string, n int, bound bool) (int, error) {
	switch {
	case bound || scope != "":
		return etagOffset(binding, token, n)
	case token == "":
		return 0, nil
	}
	start, err := strconv.Atoi(token)
	if err != nil || start < 0 || start > n {
		return 0, connect.NewError(connect.CodeInvalidArgument, "page_token is not a token of this list")
	}
	return start, nil
}

// nextToken is the token of the page that starts at end; see pageStart.
func nextToken(binding, scope string, end int, bound bool) string {
	if bound || scope != "" {
		return etagToken(binding, end)
	}
	return strconv.Itoa(end)
}

// errSortNotLive is the answer of a live list when it is asked to sort and
// the live reader does not exist yet: the stored list has name order only.
func errSortNotLive(what string) error {
	return connect.NewError(connect.CodeUnimplemented, what+" cannot be sorted yet: sorting is served by the live reader, which this deployment does not have")
}

func sortDeprecations(in []*cloudv1.DeprecationFinding, by cloudv1.DeprecationSortBy, desc bool) []*cloudv1.DeprecationFinding {
	out := slices.Clone(in)
	var f func(a, b *cloudv1.DeprecationFinding) int
	switch by {
	case cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_NAMESPACE:
		f = func(a, b *cloudv1.DeprecationFinding) int {
			return cmp.Or(cmp.Compare(a.GetNamespace(), b.GetNamespace()), cmp.Compare(a.GetKind(), b.GetKind()), cmp.Compare(a.GetName(), b.GetName()))
		}
	case cloudv1.DeprecationSortBy_DEPRECATION_SORT_BY_KIND:
		f = func(a, b *cloudv1.DeprecationFinding) int {
			return cmp.Or(cmp.Compare(a.GetKind(), b.GetKind()), cmp.Compare(a.GetNamespace(), b.GetNamespace()), cmp.Compare(a.GetName(), b.GetName()))
		}
	default: // removed first
		f = func(a, b *cloudv1.DeprecationFinding) int { return cmp.Compare(removedRank(a), removedRank(b)) }
	}
	slices.SortStableFunc(out, directed(desc, f))
	return out
}

func removedRank(f *cloudv1.DeprecationFinding) int {
	if f.GetRemoved() {
		return 0
	}
	return 1
}

// sortRecommendations orders the containers by the key. The default key and
// direction (namespace, ascending) is the list's own order, left alone.
func sortRecommendations(in []*cloudv1.ResourceRecommendation, by cloudv1.RecommendationSortBy, desc bool) []*cloudv1.ResourceRecommendation {
	out := slices.Clone(in)
	switch by {
	case cloudv1.RecommendationSortBy_RECOMMENDATION_SORT_BY_CPU_CHANGE:
		sortByChange(out, desc, func(r *cloudv1.ResourceRecommendation) int64 { return r.GetDifference().GetCpuMillis() })
	case cloudv1.RecommendationSortBy_RECOMMENDATION_SORT_BY_MEMORY_CHANGE:
		sortByChange(out, desc, func(r *cloudv1.ResourceRecommendation) int64 { return r.GetDifference().GetMemoryBytes() })
	default:
		if desc {
			slices.SortStableFunc(out, directed(true, func(a, b *cloudv1.ResourceRecommendation) int {
				return cmp.Or(cmp.Compare(a.GetNamespace(), b.GetNamespace()), cmp.Compare(a.GetWorkloadKind(), b.GetWorkloadKind()),
					cmp.Compare(a.GetWorkloadName(), b.GetWorkloadName()), cmp.Compare(a.GetContainer(), b.GetContainer()))
			}))
		}
	}
	return out
}

// sortByChange orders by the size of the change; containers without a
// recommendation come last in either direction.
func sortByChange(rows []*cloudv1.ResourceRecommendation, desc bool, delta func(*cloudv1.ResourceRecommendation) int64) {
	size := func(r *cloudv1.ResourceRecommendation) int64 { return max(delta(r), -delta(r)) }
	slices.SortStableFunc(rows, func(a, b *cloudv1.ResourceRecommendation) int {
		aHas, bHas := a.GetRecommendedRequests() != nil, b.GetRecommendedRequests() != nil
		if aHas != bHas {
			if aHas {
				return -1
			}
			return 1
		}
		return directed(desc, cmp.Compare[int64])(size(a), size(b))
	})
}
