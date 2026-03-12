package credcache

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// CacheAuthenticator orchestrates credential verification using a cache,
// a credential store (DynamoDB), singleflight for deduplication, and a
// circuit breaker for store degradation.
type CacheAuthenticator struct {
	cache *Cache
	store CredentialStore
	sf    singleflight.Group

	// Circuit breaker state (all accessed atomically).
	consecutiveFailures atomic.Int64
	lastFailure         atomic.Int64 // unix nanoseconds
	failureThreshold    int64
	cooldownDuration    time.Duration

	// Negative caching TTL for unknown users.
	negativeTTL time.Duration
}

// AuthOption configures a CacheAuthenticator.
type AuthOption func(*CacheAuthenticator)

// WithFailureThreshold sets the number of consecutive store failures before
// the circuit breaker trips.
func WithFailureThreshold(n int64) AuthOption {
	return func(a *CacheAuthenticator) {
		a.failureThreshold = n
	}
}

// WithCooldownDuration sets the duration after which the circuit breaker
// allows a retry after tripping.
func WithCooldownDuration(d time.Duration) AuthOption {
	return func(a *CacheAuthenticator) {
		a.cooldownDuration = d
	}
}

// WithNegativeTTL sets the TTL for negative cache entries (unknown users).
func WithNegativeTTL(d time.Duration) AuthOption {
	return func(a *CacheAuthenticator) {
		a.negativeTTL = d
	}
}

// NewCacheAuthenticator creates a CacheAuthenticator with the given cache and store.
// Defaults: failureThreshold=3, cooldownDuration=10s, negativeTTL=60s.
func NewCacheAuthenticator(cache *Cache, store CredentialStore, opts ...AuthOption) *CacheAuthenticator {
	a := &CacheAuthenticator{
		cache:            cache,
		store:            store,
		failureThreshold: 3,
		cooldownDuration: 10 * time.Second,
		negativeTTL:      60 * time.Second,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Verify checks the given username and password against cached or fetched credentials.
// Password is hex-encoded before constant-time comparison with the stored value.
// Returns (true, nil) on match, (false, nil) on mismatch or unknown user,
// (false, error) on store/circuit-breaker errors.
func (a *CacheAuthenticator) Verify(ctx context.Context, username string, password []byte) (bool, error) {
	if username == "" {
		return false, nil
	}

	// Try cache first.
	cred, ok := a.cache.Get(username)
	if ok {
		// Negative cache entry: reject without password comparison.
		if cred.Negative {
			return false, nil
		}
		return comparePassword(cred.Password, password), nil
	}

	// Cache miss: fetch via singleflight.
	cred, err := a.fetchWithSingleflight(ctx, username)
	if err != nil {
		return false, err
	}
	if cred == nil {
		return false, nil
	}

	return comparePassword(cred.Password, password), nil
}

// fetchWithSingleflight wraps store.Fetch with singleflight deduplication
// and circuit breaker logic.
func (a *CacheAuthenticator) fetchWithSingleflight(ctx context.Context, username string) (*Credential, error) {
	if a.IsDegraded() {
		return nil, fmt.Errorf("dynamodb circuit breaker open")
	}

	v, err, _ := a.sf.Do(username, func() (interface{}, error) {
		cred, err := a.store.Fetch(ctx, username)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Not found: store negative entry to avoid repeated lookups.
				negCred := &Credential{Username: username, Negative: true}
				a.cache.SetWithTTL(username, negCred, a.negativeTTL)
				return nil, nil
			}
			a.recordFailure()
			return nil, err
		}
		a.recordSuccess()
		a.cache.Set(username, cred)
		return cred, nil
	})

	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return v.(*Credential), nil
}

// comparePassword performs a constant-time comparison of the raw password
// bytes against the stored password string.
func comparePassword(stored string, raw []byte) bool {
	return subtle.ConstantTimeCompare(raw, []byte(stored)) == 1
}

// ResetCircuitBreaker resets the consecutive failure counter to zero.
// Used by admin API to manually recover from degraded state.
func (a *CacheAuthenticator) ResetCircuitBreaker() {
	prev := a.consecutiveFailures.Swap(0)
	if prev > 0 {
		log.Printf("[INFO] Circuit breaker reset by admin (was at %d consecutive failures)", prev)
	}
}

// IsDegraded returns true when the circuit breaker is open.
func (a *CacheAuthenticator) IsDegraded() bool {
	failures := a.consecutiveFailures.Load()
	if failures < a.failureThreshold {
		return false
	}
	lastFail := time.Unix(0, a.lastFailure.Load())
	return time.Since(lastFail) < a.cooldownDuration
}

// recordFailure increments the consecutive failure counter and records the
// failure timestamp.
func (a *CacheAuthenticator) recordFailure() {
	a.consecutiveFailures.Add(1)
	a.lastFailure.Store(time.Now().UnixNano())
}

// recordSuccess resets the failure counter. Logs restoration if recovering
// from failures.
func (a *CacheAuthenticator) recordSuccess() {
	prev := a.consecutiveFailures.Swap(0)
	if prev > 0 {
		log.Printf("[INFO] DynamoDB connectivity restored (was at %d consecutive failures)", prev)
	}
}
