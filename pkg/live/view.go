package live

import (
	"cmp"
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// tokenPrefix starts every live page token: "live:<view-id>:<offset>".
const tokenPrefix = "live:"

// buildKey is what a build serves: the tenant ID is part of it, so two
// tenants never share a build even if cluster IDs collided (7.2).
type buildKey struct{ tenant, cluster, kind string }

// cachedBuild is a build the store keeps, with its views.
type cachedBuild struct {
	key     buildKey
	build   *nodeBuild
	builtAt time.Time
	views   map[string]*pagedView
	bytes   int64
	elem    *list.Element
}

// pagedView is one sorted, filtered list over a build, paged by tokens.
type pagedView struct {
	id     string
	parent *cachedBuild
	scope  viewScope
	rows   []*cloudv1.NodeGroupNode
	usedAt time.Time
}

// viewScope is what a token is bound to: a request with another tenant,
// cluster, group, filter or sort refuses the token ("restart the list").
type viewScope struct {
	tenant, cluster, group, nameContains string
	filter                               cloudv1.NodeStatusFilter
	sortBy                               cloudv1.NodeSortBy
	desc                                 bool
}

// store keeps the builds and their views in memory, bounded by bytes: the
// least recently used build goes first, with its views. A restart loses
// them all; their tokens then answer "restart the list".
type store struct {
	mu     sync.Mutex
	lim    Limits
	builds map[buildKey]*cachedBuild
	views  map[string]*pagedView
	lru    *list.List // of *cachedBuild, most recently used first
	bytes  int64
}

func newStore(lim Limits) *store {
	return &store{lim: lim, builds: map[buildKey]*cachedBuild{}, views: map[string]*pagedView{}, lru: list.New()}
}

// fresh is the build of key made within the reuse window; nil for none.
func (s *store) fresh(key buildKey, now time.Time) *cachedBuild {
	s.mu.Lock()
	defer s.mu.Unlock()
	cb := s.builds[key]
	if cb == nil || now.Sub(cb.builtAt) > s.lim.Reuse {
		return nil
	}
	s.lru.MoveToFront(cb.elem)
	return cb
}

// put keeps b as key's build (the one before it stays only for its views'
// remaining life), then evicts down to the byte bound.
func (s *store) put(key buildKey, b *nodeBuild, now time.Time) *cachedBuild {
	s.mu.Lock()
	defer s.mu.Unlock()
	cb := &cachedBuild{key: key, build: b, builtAt: now, views: map[string]*pagedView{}, bytes: b.bytes}
	if old := s.builds[key]; old != nil && len(old.views) == 0 {
		s.dropLocked(old)
	}
	s.builds[key] = cb
	cb.elem = s.lru.PushFront(cb)
	s.bytes += cb.bytes
	s.evictLocked(cb)
	return cb
}

// newView keeps rows as a view of cb under scope and returns its ID; "" when
// cb was evicted meanwhile (the rows are served once, without a token).
func (s *store) newView(cb *cachedBuild, scope viewScope, rows []*cloudv1.NodeGroupNode, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cb.elem == nil {
		return ""
	}
	v := &pagedView{id: newViewID(), parent: cb, scope: scope, rows: rows, usedAt: now}
	cost := int64(8 * len(rows))
	cb.views[v.id], s.views[v.id] = v, v
	cb.bytes += cost
	s.bytes += cost
	s.lru.MoveToFront(cb.elem)
	s.evictLocked(cb)
	return v.id
}

// view is the live view id, marked used; false when it is gone: idle longer
// than ViewIdle, its build older than ViewMaxAge, evicted, or unknown.
func (s *store) view(id string, now time.Time) (*pagedView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.views[id]
	if v == nil {
		return nil, false
	}
	if now.Sub(v.usedAt) > s.lim.ViewIdle || now.Sub(v.parent.builtAt) > s.lim.ViewMaxAge {
		s.dropViewLocked(v)
		return nil, false
	}
	v.usedAt = now
	s.lru.MoveToFront(v.parent.elem)
	return v, true
}

// sweep drops expired views, and builds that serve nothing any more.
func (s *store) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.views {
		if now.Sub(v.usedAt) > s.lim.ViewIdle || now.Sub(v.parent.builtAt) > s.lim.ViewMaxAge {
			s.dropViewLocked(v)
		}
	}
	// The list, not the map: a build replaced while its views lived is only
	// in the list.
	for e := s.lru.Front(); e != nil; {
		next := e.Next()
		if cb := e.Value.(*cachedBuild); len(cb.views) == 0 && now.Sub(cb.builtAt) > s.lim.Reuse {
			s.dropLocked(cb)
		}
		e = next
	}
}

// size is the bytes kept, for the metrics and the tests.
func (s *store) size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

func (s *store) evictLocked(keep *cachedBuild) {
	for s.bytes > s.lim.ViewBytes {
		back := s.lru.Back()
		if back == nil {
			return
		}
		cb := back.Value.(*cachedBuild)
		if cb == keep && s.lru.Len() == 1 {
			return // the one being served stays, even alone over the bound
		}
		s.dropLocked(cb)
	}
}

func (s *store) dropViewLocked(v *pagedView) {
	delete(s.views, v.id)
	if _, ok := v.parent.views[v.id]; ok {
		delete(v.parent.views, v.id)
		cost := int64(8 * len(v.rows))
		v.parent.bytes -= cost
		if v.parent.elem != nil {
			s.bytes -= cost
		}
	}
}

func (s *store) dropLocked(cb *cachedBuild) {
	if cb.elem == nil {
		return
	}
	for _, v := range cb.views {
		delete(s.views, v.id)
	}
	cb.views = map[string]*pagedView{}
	s.lru.Remove(cb.elem)
	cb.elem = nil
	s.bytes -= cb.bytes
	if s.builds[cb.key] == cb {
		delete(s.builds, cb.key)
	}
}

// newViewID is 128 random bits.
func newViewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func viewToken(id string, offset int) string { return tokenPrefix + id + ":" + strconv.Itoa(offset) }

// parseToken is a live token's view ID and offset; false for anything else
// (a stored list's decimal token among them).
func parseToken(token string) (string, int, bool) {
	rest, ok := strings.CutPrefix(token, tokenPrefix)
	if !ok {
		return "", 0, false
	}
	id, off, ok := strings.Cut(rest, ":")
	if !ok || len(id) != 32 {
		return "", 0, false
	}
	n, err := strconv.Atoi(off)
	if err != nil || n < 0 {
		return "", 0, false
	}
	return id, n, true
}

// selectNodes are the build's rows of the scope's group that pass its
// filter, sorted by its key with the node name as tie-breaker.
func selectNodes(b *nodeBuild, sc viewScope) []*cloudv1.NodeGroupNode {
	needle := strings.ToLower(sc.nameContains)
	var out []*cloudv1.NodeGroupNode
	for _, r := range b.rows {
		if r.group != sc.group {
			continue
		}
		switch sc.filter {
		case cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_READY:
			if !r.node.GetReady() {
				continue
			}
		case cloudv1.NodeStatusFilter_NODE_STATUS_FILTER_NOT_READY:
			if r.node.GetReady() {
				continue
			}
		default: // every node
		}
		if needle != "" && !strings.Contains(strings.ToLower(r.node.GetName()), needle) {
			continue
		}
		out = append(out, r.node)
	}
	sortNodes(out, sc.sortBy, sc.desc)
	return out
}

// sortNodes orders rows by key (descending with desc), then by name. AGE
// ascending is oldest first (creation time); READINESS ascending is not Ready
// first; the used percentages put rows without usage last in either order.
func sortNodes(rows []*cloudv1.NodeGroupNode, by cloudv1.NodeSortBy, desc bool) {
	key := func(a, b *cloudv1.NodeGroupNode) int { return 0 }
	switch by {
	case cloudv1.NodeSortBy_NODE_SORT_BY_AGE:
		key = func(a, b *cloudv1.NodeGroupNode) int {
			return cmp.Compare(a.GetCreatedAt().AsTime().UnixNano(), b.GetCreatedAt().AsTime().UnixNano())
		}
	case cloudv1.NodeSortBy_NODE_SORT_BY_READINESS:
		key = func(a, b *cloudv1.NodeGroupNode) int {
			return cmp.Compare(boolInt(a.GetReady()), boolInt(b.GetReady()))
		}
	case cloudv1.NodeSortBy_NODE_SORT_BY_CPU_REQUESTED_PERCENT:
		key = func(a, b *cloudv1.NodeGroupNode) int {
			return cmp.Compare(percent(a.GetRequestedCpuMillis(), a.GetAllocatableCpuMillis()), percent(b.GetRequestedCpuMillis(), b.GetAllocatableCpuMillis()))
		}
	case cloudv1.NodeSortBy_NODE_SORT_BY_MEMORY_REQUESTED_PERCENT:
		key = func(a, b *cloudv1.NodeGroupNode) int {
			return cmp.Compare(percent(a.GetRequestedMemoryBytes(), a.GetAllocatableMemoryBytes()), percent(b.GetRequestedMemoryBytes(), b.GetAllocatableMemoryBytes()))
		}
	case cloudv1.NodeSortBy_NODE_SORT_BY_POD_COUNT:
		key = func(a, b *cloudv1.NodeGroupNode) int { return cmp.Compare(a.GetPodCount(), b.GetPodCount()) }
	default: // NAME, and the used percentages (usedKey): the name alone
	}
	directedKey := key
	if desc {
		directedKey = func(a, b *cloudv1.NodeGroupNode) int { return -key(a, b) }
	}
	used := usedKey(by)
	slices.SortStableFunc(rows, func(a, b *cloudv1.NodeGroupNode) int {
		if used != nil {
			// Rows without usage come last, whatever the direction.
			ha, hb := used.has(a), used.has(b)
			if ha != hb {
				if ha {
					return -1
				}
				return 1
			}
			if ha {
				c := cmp.Compare(used.value(a), used.value(b))
				if desc {
					c = -c
				}
				if c != 0 {
					return c
				}
			}
			return strings.Compare(a.GetName(), b.GetName())
		}
		if c := directedKey(a, b); c != 0 {
			return c
		}
		return strings.Compare(a.GetName(), b.GetName())
	})
}

type usedSort struct {
	has   func(*cloudv1.NodeGroupNode) bool
	value func(*cloudv1.NodeGroupNode) float64
}

func usedKey(by cloudv1.NodeSortBy) *usedSort {
	switch by {
	case cloudv1.NodeSortBy_NODE_SORT_BY_CPU_USED_PERCENT:
		return &usedSort{has: func(n *cloudv1.NodeGroupNode) bool { return n.UsedCpuMillis != nil },
			value: func(n *cloudv1.NodeGroupNode) float64 {
				return percent(n.GetUsedCpuMillis(), n.GetAllocatableCpuMillis())
			}}
	case cloudv1.NodeSortBy_NODE_SORT_BY_MEMORY_USED_PERCENT:
		return &usedSort{has: func(n *cloudv1.NodeGroupNode) bool { return n.UsedMemoryBytes != nil },
			value: func(n *cloudv1.NodeGroupNode) float64 {
				return percent(n.GetUsedMemoryBytes(), n.GetAllocatableMemoryBytes())
			}}
	default:
		return nil
	}
}

// percent is part of whole in percent; a node with nothing allocatable is 0.
func percent(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
