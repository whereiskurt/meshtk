package keycache

import (
	"sort"
	"time"

	"github.com/maypok86/otter/v2"
	"github.com/maypok86/otter/v2/stats"
)

// CacheStats holds cache performance statistics.
type CacheStats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64
	HitRate   float64
}

// Cache wraps an Otter v2 cache for typed pubkey access, keyed by the canonical
// nodeId string ("!%08x"). Mirrors credcache.Cache but stores *Key values.
type Cache struct {
	inner   *otter.Cache[string, *Key]
	counter *stats.Counter
}

// NewCache creates a new pubkey cache with the given TTL (seconds) and approximate
// max size (MB). maxSizeMB is converted to an approximate entry count (~100 bytes
// per key). For keycache the TTL is 60-120s (plain expiry, NOT credcache's 900s).
func NewCache(ttlSecs int, maxSizeMB int) (*Cache, error) {
	counter := stats.NewCounter()
	// Approximate: ~100 bytes per key, so 1 MB ~ 10,000 entries
	maxEntries := maxSizeMB * 10000
	if maxEntries < 100 {
		maxEntries = 100
	}

	inner, err := otter.New[string, *Key](&otter.Options[string, *Key]{
		MaximumSize:      maxEntries,
		ExpiryCalculator: otter.ExpiryWriting[string, *Key](time.Duration(ttlSecs) * time.Second),
		StatsRecorder:    counter,
	})
	if err != nil {
		return nil, err
	}

	return &Cache{
		inner:   inner,
		counter: counter,
	}, nil
}

// Get retrieves a key by nodeId. Returns nil, false on cache miss.
func (c *Cache) Get(nodeID string) (*Key, bool) {
	return c.inner.GetIfPresent(nodeID)
}

// Set stores a key in the cache keyed by nodeId.
func (c *Cache) Set(nodeID string, key *Key) {
	c.inner.Set(nodeID, key)
}

// Delete removes a key from the cache.
func (c *Cache) Delete(nodeID string) {
	c.inner.Invalidate(nodeID)
}

// Stats returns a snapshot of cache performance statistics.
func (c *Cache) Stats() CacheStats {
	s := c.inner.Stats()
	return CacheStats{
		Hits:      s.Hits,
		Misses:    s.Misses,
		Evictions: s.Evictions,
		HitRate:   s.HitRatio(),
	}
}

// Size returns the approximate number of entries in the cache.
func (c *Cache) Size() int {
	return c.inner.EstimatedSize()
}

// CacheEntry represents a single cache entry for admin listing.
type CacheEntry struct {
	NodeID       string `json:"node_id"`
	TTLRemaining int    `json:"ttl_remaining"`
	Negative     bool   `json:"negative"`
}

// SetWithTTL stores a key with a custom TTL duration (used for negative entries).
func (c *Cache) SetWithTTL(nodeID string, key *Key, ttl time.Duration) {
	c.inner.Set(nodeID, key)
	c.inner.SetExpiresAfter(nodeID, ttl)
}

// DeleteAll invalidates all cache entries, returning the approximate count evicted.
func (c *Cache) DeleteAll() int {
	count := c.inner.EstimatedSize()
	c.inner.InvalidateAll()
	return count
}

// Entries returns all cache entries sorted by TTL remaining (ascending).
// Returns an empty slice (not nil) when the cache is empty.
func (c *Cache) Entries() []CacheEntry {
	entries := make([]CacheEntry, 0)

	for nodeID := range c.inner.All() {
		entry, ok := c.inner.GetEntryQuietly(nodeID)
		if !ok {
			continue
		}
		expiresAt := time.Unix(0, entry.ExpiresAtNano)
		ttl := int(time.Until(expiresAt).Seconds())
		if ttl < 0 {
			ttl = 0
		}
		entries = append(entries, CacheEntry{
			NodeID:       nodeID,
			TTLRemaining: ttl,
			Negative:     entry.Value.Negative,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TTLRemaining < entries[j].TTLRemaining
	})

	return entries
}

// Close stops all cache goroutines and releases resources.
func (c *Cache) Close() {
	c.inner.StopAllGoroutines()
}
