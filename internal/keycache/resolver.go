package keycache

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// KeyResolver resolves a sender node's authoritative decrypt pubkey using a cache,
// a KeyStore (DynamoDB GetItem), singleflight for deduplication, and a circuit
// breaker for store degradation. It mirrors credcache.CacheAuthenticator.
//
// ONE KeyResolver is shared process-wide across the entire ghost fleet (the
// fleet-wide generalization of crypto.go's pubKeyCache sync.Map). It is the
// load-bounded key source: cache-first, at most ~one DDB read per node per TTL,
// independent of packet/client volume — never per-packet, never per-client.
type KeyResolver struct {
	cache *Cache
	store KeyStore
	sf    singleflight.Group

	// Circuit breaker state (all accessed atomically).
	consecutiveFailures atomic.Int64
	lastFailure         atomic.Int64 // unix nanoseconds
	failureThreshold    int64
	cooldownDuration    time.Duration

	// Negative caching TTL for unknown (unenrolled) senders.
	negativeTTL time.Duration
}

// ResolverOption configures a KeyResolver.
type ResolverOption func(*KeyResolver)

// WithFailureThreshold sets the number of consecutive store failures before
// the circuit breaker trips.
func WithFailureThreshold(n int64) ResolverOption {
	return func(r *KeyResolver) {
		r.failureThreshold = n
	}
}

// WithCooldownDuration sets the duration after which the circuit breaker
// allows a retry after tripping.
func WithCooldownDuration(d time.Duration) ResolverOption {
	return func(r *KeyResolver) {
		r.cooldownDuration = d
	}
}

// WithNegativeTTL sets the TTL for negative cache entries (unknown senders).
func WithNegativeTTL(d time.Duration) ResolverOption {
	return func(r *KeyResolver) {
		r.negativeTTL = d
	}
}

// NewKeyResolver creates a KeyResolver with the given cache and store.
// Defaults mirror credcache: failureThreshold=3, cooldownDuration=10s,
// negativeTTL=60s. The positive-entry TTL is owned by the cache (NewCache,
// 60-120s), NOT credcache's 900s.
func NewKeyResolver(cache *Cache, store KeyStore, opts ...ResolverOption) *KeyResolver {
	r := &KeyResolver{
		cache:            cache,
		store:            store,
		failureThreshold: 3,
		cooldownDuration: 10 * time.Second,
		negativeTTL:      60 * time.Second,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Resolve returns the sender node's 0x-hex pubkey, cache-first.
//   - cache hit (positive) → (hex, true, nil)
//   - cache hit (negative) → ("", false, nil), no store call
//   - cache miss → singleflight fetch; ErrNotFound negative-caches and returns
//     ("", false, nil); store error returns ("", false, err)
//
// A miss (ok=false, err=nil) is the caller's cue to apply the fallback flag
// (nodes.json bring-up) or NACK (fallback=none, poisoning-resistant).
func (r *KeyResolver) Resolve(ctx context.Context, nodeNum uint32) (string, bool, error) {
	if nodeNum == 0 {
		return "", false, nil
	}

	nodeID := NodeIDFromNum(nodeNum)

	// Try cache first.
	if key, ok := r.cache.Get(nodeID); ok {
		if key.Negative {
			return "", false, nil
		}
		return key.PubKeyHex, true, nil
	}

	// Cache miss: fetch via singleflight.
	key, err := r.fetchWithSingleflight(ctx, nodeNum, nodeID)
	if err != nil {
		return "", false, err
	}
	if key == nil {
		return "", false, nil
	}
	return key.PubKeyHex, true, nil
}

// fetchWithSingleflight wraps store.Fetch with singleflight deduplication and
// circuit breaker logic. Concurrent misses for the same node collapse to one
// store call.
func (r *KeyResolver) fetchWithSingleflight(ctx context.Context, nodeNum uint32, nodeID string) (*Key, error) {
	if r.IsDegraded() {
		return nil, fmt.Errorf("dynamodb circuit breaker open")
	}

	v, err, _ := r.sf.Do(nodeID, func() (interface{}, error) {
		key, err := r.store.Fetch(ctx, nodeNum)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Unknown sender: negative-cache to bound DDB reads for
				// unenrolled nodes over the negativeTTL window.
				negKey := &Key{NodeID: nodeID, NodeNum: nodeNum, Negative: true}
				r.cache.SetWithTTL(nodeID, negKey, r.negativeTTL)
				return nil, nil
			}
			r.recordFailure()
			return nil, err
		}
		r.recordSuccess()
		r.cache.Set(nodeID, key)
		return key, nil
	})

	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return v.(*Key), nil
}

// ResetCircuitBreaker resets the consecutive failure counter to zero.
// Used by admin API to manually recover from degraded state.
func (r *KeyResolver) ResetCircuitBreaker() {
	prev := r.consecutiveFailures.Swap(0)
	if prev > 0 {
		log.Printf("[INFO] keycache circuit breaker reset by admin (was at %d consecutive failures)", prev)
	}
}

// IsDegraded returns true when the circuit breaker is open.
func (r *KeyResolver) IsDegraded() bool {
	failures := r.consecutiveFailures.Load()
	if failures < r.failureThreshold {
		return false
	}
	lastFail := time.Unix(0, r.lastFailure.Load())
	return time.Since(lastFail) < r.cooldownDuration
}

// recordFailure increments the consecutive failure counter and records the
// failure timestamp.
func (r *KeyResolver) recordFailure() {
	r.consecutiveFailures.Add(1)
	r.lastFailure.Store(time.Now().UnixNano())
}

// recordSuccess resets the failure counter. Logs restoration if recovering
// from failures.
func (r *KeyResolver) recordSuccess() {
	prev := r.consecutiveFailures.Swap(0)
	if prev > 0 {
		log.Printf("[INFO] keycache DynamoDB connectivity restored (was at %d consecutive failures)", prev)
	}
}
