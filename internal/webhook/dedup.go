package webhook

import (
	"container/list"
	"sync"
	"time"
)

// DedupCache remembers the (sessionID, messageID) pairs whose /message request
// has already been recorded, so a retry returns a cached ack instead of writing
// the user frame to the container's stdin a second time. CheckAndRecord
// atomically checks whether a pair is already present and records it in the
// same step, closing the race a split check-then-record would leave between
// two concurrent retries. A caller that gets a fresh (non-duplicate) result
// from CheckAndRecord and then fails to deliver the frame must call Rollback,
// or the cache would keep the record and silently swallow a legitimate retry
// as a duplicate. It is TTL- and capacity-bounded; an empty messageID NEVER
// dedups (the client opted out of at-most-once delivery). All methods are safe
// for concurrent use.
type DedupCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	now      func() time.Time

	entries *list.List
	index   map[string]*list.Element
}

type dedupEntry struct {
	key    string
	stored time.Time
}

type dedupCacheOption func(*DedupCache)

// NewDedupCache builds a dedup cache with the given TTL and capacity. A TTL
// <= 0 disables time-based expiry; a capacity <= 0 disables the hard cap.
func NewDedupCache(ttl time.Duration, capacity int, opts ...dedupCacheOption) *DedupCache {
	c := &DedupCache{
		ttl:      ttl,
		capacity: capacity,
		now:      time.Now,
		entries:  list.New(),
		index:    make(map[string]*list.Element),
	}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// dedupKey builds the composite lookup key. A NUL delimiter cannot appear in a
// validated session ID, so fields never collide across the boundary.
func dedupKey(sessionID, messageID string) string {
	return sessionID + "\x00" + messageID
}

// CheckAndRecord atomically checks whether the (sessionID, messageID) pair is
// already present and, if not, records it. It returns true when the pair was
// already in the cache inside the TTL window (the caller should return a
// cached ack), and false when the pair was fresh and has now been recorded
// (the caller must attempt delivery). An empty messageID always returns false
// and records nothing. Callers that get false must call Rollback on delivery
// failure so a legitimate retry is not permanently silenced.
func (c *DedupCache) CheckAndRecord(sessionID, messageID string) bool {
	if messageID == "" {
		return false
	}

	key := dedupKey(sessionID, messageID)

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	if el, ok := c.index[key]; ok {
		entry := el.Value.(*dedupEntry)
		if c.ttl <= 0 || now.Sub(entry.stored) <= c.ttl {
			return true // already delivered inside TTL window
		}
		// Expired: reap and fall through to record a fresh entry.
		c.entries.Remove(el)
		delete(c.index, key)
	}

	el := c.entries.PushBack(&dedupEntry{key: key, stored: now})
	c.index[key] = el

	if c.capacity > 0 {
		for c.entries.Len() > c.capacity {
			oldest := c.entries.Front()
			if oldest == nil {
				break
			}

			c.entries.Remove(oldest)
			delete(c.index, oldest.Value.(*dedupEntry).key)
		}
	}

	return false
}

// Rollback removes a just-recorded (sessionID, messageID) pair from the cache.
// It is called when a delivery fails after CheckAndRecord has already recorded
// the pair, so a subsequent retry can still be delivered. An empty messageID
// is a no-op.
func (c *DedupCache) Rollback(sessionID, messageID string) {
	if messageID == "" {
		return
	}

	key := dedupKey(sessionID, messageID)

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.index[key]; ok {
		c.entries.Remove(el)
		delete(c.index, key)
	}
}
