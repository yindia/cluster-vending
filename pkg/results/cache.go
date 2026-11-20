package results

import (
	"container/list"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
)

// Cache keeps the newest objects a reader has read, by bucket, key and
// message type, with the ETag they were read at: a read sends the ETag
// (If-None-Match) and S3 answers 304 without the body while the object is
// the same. Reading is still authorized by S3 on every call, so a tenant
// that lost access is denied, not served from here. Safe for concurrent
// use; the least recently used entry goes first.
type Cache struct {
	mu      sync.Mutex
	max     int
	order   *list.List // *entry, most recent first
	entries map[string]*list.Element
}

type entry struct {
	key       string
	etag      string
	collected time.Time
	msg       proto.Message
}

// NewCache returns a cache of at most max objects (zero: DefaultCacheEntries).
func NewCache(max int) *Cache {
	if max <= 0 {
		max = DefaultCacheEntries
	}
	return &Cache{max: max, order: list.New(), entries: map[string]*list.Element{}}
}

func cacheKey(bucket, key string, msg proto.Message) string {
	return bucket + "\\x00" + key + "\\x00" + string(msg.ProtoReflect().Descriptor().FullName())
}

// get is the entry of key; a copy of its message goes into into.
func (c *Cache) get(key string, into proto.Message) (etag string, collected time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, found := c.entries[key]
	if !found {
		return "", time.Time{}, false
	}
	c.order.MoveToFront(el)
	e := el.Value.(*entry)
	proto.Reset(into)
	proto.Merge(into, e.msg)
	return e.etag, e.collected, true
}

// etagOf is the ETag cached for key; empty for none.
func (c *Cache) etagOf(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, found := c.entries[key]; found {
		return el.Value.(*entry).etag
	}
	return ""
}

// put keeps a copy of msg.
func (c *Cache) put(key, etag string, collected time.Time, msg proto.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := &entry{key: key, etag: etag, collected: collected, msg: proto.Clone(msg)}
	if el, found := c.entries[key]; found {
		el.Value = e
		c.order.MoveToFront(el)
		return
	}
	c.entries[key] = c.order.PushFront(e)
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.entries, last.Value.(*entry).key)
	}
}

// Len is how many objects the cache holds.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
