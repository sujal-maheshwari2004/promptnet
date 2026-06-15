package server

import (
	"sync"
	"time"

	pb "promptnet/gen/promptnet/v1"
)

// ttlCache is the Phase 2 L2 (server-side) cache: in-process, keyed by URI,
// entries expire after ttl. Only validated prompts are ever put here.
//
// TTL is the convergence mechanism: a `put` that changes a prompt is reflected
// within ttl. Content identity is the version_hash — a changed prompt yields a
// new hash, so a stale entry is always distinguishable from fresh content.
//
// ponytail: in-process map, unbounded (entries are bounded by the prompt count,
// which is small). Swap for Redis + LRU only when serving goes multi-node or the
// prompt set stops fitting in memory.
type ttlCache struct {
	ttl time.Duration
	mu  sync.RWMutex
	m   map[string]cacheEntry
}

type cacheEntry struct {
	resp   *pb.GetPromptResponse
	expiry time.Time
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{ttl: ttl, m: map[string]cacheEntry{}}
}

// A nil *ttlCache is a valid disabled cache: get always misses, put is a no-op.
func (c *ttlCache) get(uri string) (*pb.GetPromptResponse, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	e, ok := c.m[uri]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiry) {
		return nil, false
	}
	return e.resp, true
}

func (c *ttlCache) put(uri string, resp *pb.GetPromptResponse) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.m[uri] = cacheEntry{resp: resp, expiry: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}
