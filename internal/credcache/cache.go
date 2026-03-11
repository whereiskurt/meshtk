package credcache

import (
	"sort"
	"time"

	"github.com/maypok86/otter/v2"
	"github.com/maypok86/otter/v2/stats"
)

// CacheStats holds cache performance statistics.
type CacheStats struct {
	Hits       uint64
	Misses     uint64
	Evictions  uint64
	HitRate    float64
}

// Cache wraps an Otter v2 cache for typed credential access.
type Cache struct {
	inner   *otter.Cache[string, *Credential]
	counter *stats.Counter
}

// NewCache creates a new credential cache with the given TTL (seconds) and approximate max size (MB).
// maxSizeMB is converted to an approximate entry count (~100 bytes per credential).
func NewCache(ttlSecs int, maxSizeMB int) (*Cache, error) {
	counter := stats.NewCounter()
	// Approximate: ~100 bytes per credential, so 1 MB ~ 10,000 entries
	maxEntries := maxSizeMB * 10000
	if maxEntries < 100 {
		maxEntries = 100
	}

	inner, err := otter.New[string, *Credential](&otter.Options[string, *Credential]{
		MaximumSize:      maxEntries,
		ExpiryCalculator: otter.ExpiryWriting[string, *Credential](time.Duration(ttlSecs) * time.Second),
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

// Get retrieves a credential by username. Returns nil, false on cache miss.
func (c *Cache) Get(username string) (*Credential, bool) {
	return c.inner.GetIfPresent(username)
}

// Set stores a credential in the cache keyed by username.
func (c *Cache) Set(username string, cred *Credential) {
	c.inner.Set(username, cred)
}

// Delete removes a credential from the cache.
func (c *Cache) Delete(username string) {
	c.inner.Invalidate(username)
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
	Username     string `json:"username"`
	TTLRemaining int    `json:"ttl_remaining"`
	Negative     bool   `json:"negative"`
}

// SetWithTTL stores a credential with a custom TTL duration.
func (c *Cache) SetWithTTL(username string, cred *Credential, ttl time.Duration) {
	c.inner.Set(username, cred)
	c.inner.SetExpiresAfter(username, ttl)
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

	for username := range c.inner.All() {
		entry, ok := c.inner.GetEntryQuietly(username)
		if !ok {
			continue
		}
		expiresAt := time.Unix(0, entry.ExpiresAtNano)
		ttl := int(time.Until(expiresAt).Seconds())
		if ttl < 0 {
			ttl = 0
		}
		entries = append(entries, CacheEntry{
			Username:     username,
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
