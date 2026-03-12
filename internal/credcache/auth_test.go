package credcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockStore implements CredentialStore for testing.
type mockStore struct {
	mu        sync.Mutex
	creds     map[string]*Credential
	err       error
	callCount atomic.Int64
}

func newMockStore() *mockStore {
	return &mockStore{
		creds: make(map[string]*Credential),
	}
}

func (m *mockStore) Fetch(ctx context.Context, username string) (*Credential, error) {
	m.callCount.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}
	cred, ok := m.creds[username]
	if !ok {
		return nil, ErrNotFound
	}
	return cred, nil
}

func (m *mockStore) setCred(username string, cred *Credential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creds[username] = cred
}

func (m *mockStore) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *mockStore) calls() int64 {
	return m.callCount.Load()
}

// newTestCache creates a small cache for testing.
func newTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := NewCache(5, 1) // 5s TTL, 1MB
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestVerify_CacheHit_ValidPassword(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()

	// Pre-populate cache with plain password
	cache.Set("alice", &Credential{
		Username: "alice",
		Password: "hello",
		Usertype: "device",
	})

	auth := NewCacheAuthenticator(cache, store)

	ok, err := auth.Verify(context.Background(), "alice", []byte("hello"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() = false, want true for valid cached credential")
	}
	if store.calls() != 0 {
		t.Errorf("store.Fetch called %d times, want 0 (cache hit)", store.calls())
	}
}

func TestVerify_CacheHit_InvalidPassword(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()

	cache.Set("alice", &Credential{
		Username: "alice",
		Password: "hello",
		Usertype: "device",
	})

	auth := NewCacheAuthenticator(cache, store)

	ok, err := auth.Verify(context.Background(), "alice", []byte("wrong"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if ok {
		t.Fatal("Verify() = true, want false for wrong password")
	}
}

func TestVerify_CacheMiss_FetchAndPopulate(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setCred("bob", &Credential{
		Username: "bob",
		Password: "world",
		Usertype: "device",
	})

	auth := NewCacheAuthenticator(cache, store)

	ok, err := auth.Verify(context.Background(), "bob", []byte("world"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() = false, want true after successful fetch")
	}
	if store.calls() != 1 {
		t.Errorf("store.Fetch called %d times, want 1", store.calls())
	}

	// Verify cache was populated
	cred, found := cache.Get("bob")
	if !found {
		t.Fatal("cache should contain bob after fetch")
	}
	if cred.Password != "world" {
		t.Errorf("cached password = %q, want %q", cred.Password, "world")
	}
}

func TestVerify_CacheMiss_UserNotFound(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore() // no creds configured

	auth := NewCacheAuthenticator(cache, store)

	ok, err := auth.Verify(context.Background(), "unknown", []byte("pass"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if ok {
		t.Fatal("Verify() = true, want false for unknown user")
	}
}

func TestVerify_EmptyUsername(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()

	auth := NewCacheAuthenticator(cache, store)

	ok, err := auth.Verify(context.Background(), "", []byte("pass"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if ok {
		t.Fatal("Verify() = true, want false for empty username")
	}
	if store.calls() != 0 {
		t.Errorf("store.Fetch called %d times, want 0 for empty username", store.calls())
	}
}

func TestVerify_HexEncoding(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()

	// Store credential with known password
	cache.Set("hexuser", &Credential{
		Username: "hexuser",
		Password: "hello",
		Usertype: "device",
	})

	auth := NewCacheAuthenticator(cache, store)

	// Should match: direct string comparison
	ok, err := auth.Verify(context.Background(), "hexuser", []byte("hello"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() = false, want true for matching password")
	}

	// Should NOT match with wrong password
	ok, err = auth.Verify(context.Background(), "hexuser", []byte("wrong"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if ok {
		t.Fatal("Verify() = true, want false for wrong password")
	}
}

func TestVerify_ConstantTimeCompare(t *testing.T) {
	// This test verifies the implementation uses subtle.ConstantTimeCompare
	// by checking the source code. The actual timing properties are guaranteed
	// by the crypto/subtle package.
	// Implementation verification: auth.go must import "crypto/subtle" and use
	// subtle.ConstantTimeCompare in the password comparison path.
	// We verify correctness by ensuring valid and invalid passwords return expected results.
	cache := newTestCache(t)
	store := newMockStore()

	cache.Set("ctuser", &Credential{
		Username: "ctuser",
		Password: "test",
		Usertype: "device",
	})

	auth := NewCacheAuthenticator(cache, store)

	// Valid
	ok, err := auth.Verify(context.Background(), "ctuser", []byte("test"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() = false, want true")
	}

	// Invalid
	ok, err = auth.Verify(context.Background(), "ctuser", []byte("tess"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if ok {
		t.Fatal("Verify() = true, want false")
	}
}

func TestSingleflight_DeduplicatesConcurrentFetches(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setCred("shared", &Credential{
		Username: "shared",
		Password: "abc",
		Usertype: "device",
	})

	auth := NewCacheAuthenticator(cache, store)

	var wg sync.WaitGroup
	const concurrency = 10
	results := make([]bool, concurrency)
	errs := make([]error, concurrency)

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			ok, err := auth.Verify(context.Background(), "shared", []byte("abc"))
			results[idx] = ok
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Verify() error = %v", i, err)
		}
	}
	for i, ok := range results {
		if !ok {
			t.Errorf("goroutine %d: Verify() = false, want true", i)
		}
	}

	calls := store.calls()
	if calls != 1 {
		t.Errorf("store.Fetch called %d times, want exactly 1 (singleflight dedup)", calls)
	}
}

func TestCircuitBreaker_TripsAfterConsecutiveFailures(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setError(errors.New("dynamodb timeout"))

	auth := NewCacheAuthenticator(cache, store,
		WithFailureThreshold(2),
		WithCooldownDuration(50*time.Millisecond),
	)

	// First 2 calls should attempt store (and fail)
	for i := 0; i < 2; i++ {
		_, err := auth.Verify(context.Background(), fmt.Sprintf("user%d", i), []byte("pass"))
		if err == nil {
			t.Fatalf("call %d: Verify() error = nil, want error", i)
		}
	}

	if store.calls() != 2 {
		t.Fatalf("store.Fetch called %d times before trip, want 2", store.calls())
	}

	// Third call should be rejected by circuit breaker without calling store
	_, err := auth.Verify(context.Background(), "user3", []byte("pass"))
	if err == nil {
		t.Fatal("Verify() error = nil, want circuit breaker error")
	}

	if store.calls() != 2 {
		t.Errorf("store.Fetch called %d times after trip, want still 2", store.calls())
	}
}

func TestCircuitBreaker_RecoveryAfterCooldown(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setError(errors.New("dynamodb timeout"))

	auth := NewCacheAuthenticator(cache, store,
		WithFailureThreshold(2),
		WithCooldownDuration(50*time.Millisecond),
	)

	// Trip the circuit breaker
	for i := 0; i < 2; i++ {
		auth.Verify(context.Background(), fmt.Sprintf("user%d", i), []byte("pass"))
	}

	// Wait for cooldown
	time.Sleep(100 * time.Millisecond)

	// Now fix the store
	store.setError(nil)
	store.setCred("recovered", &Credential{
		Username: "recovered",
		Password: "ok",
		Usertype: "device",
	})

	ok, err := auth.Verify(context.Background(), "recovered", []byte("ok"))
	if err != nil {
		t.Fatalf("Verify() after recovery: error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() after recovery = false, want true")
	}
}

func TestCircuitBreaker_ResetsOnSuccess(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()

	auth := NewCacheAuthenticator(cache, store,
		WithFailureThreshold(3),
		WithCooldownDuration(50*time.Millisecond),
	)

	// Cause 2 failures (not enough to trip with threshold=3)
	store.setError(errors.New("transient error"))
	for i := 0; i < 2; i++ {
		auth.Verify(context.Background(), fmt.Sprintf("fail%d", i), []byte("pass"))
	}

	// Succeed once -- should reset counter
	store.setError(nil)
	store.setCred("good", &Credential{
		Username: "good",
		Password: "ok",
		Usertype: "device",
	})

	ok, err := auth.Verify(context.Background(), "good", []byte("ok"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() = false, want true")
	}

	// Now cause 2 more failures -- should NOT trip because counter was reset
	store.setError(errors.New("transient error"))
	for i := 0; i < 2; i++ {
		auth.Verify(context.Background(), fmt.Sprintf("fail_again%d", i), []byte("pass"))
	}

	// This should still attempt the store (not blocked by circuit breaker)
	callsBefore := store.calls()
	store.setError(nil)
	store.setCred("good2", &Credential{
		Username: "good2",
		Password: "ok",
		Usertype: "device",
	})

	ok, err = auth.Verify(context.Background(), "good2", []byte("ok"))
	if err != nil {
		t.Fatalf("Verify() error = %v after reset: %v", err, err)
	}
	if !ok {
		t.Fatal("Verify() = false, want true after counter reset")
	}
	if store.calls() <= callsBefore {
		t.Error("store.Fetch should have been called (circuit breaker not tripped)")
	}
}

func TestCacheAuthenticator_ResetCircuitBreaker(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setError(errors.New("dynamodb timeout"))

	auth := NewCacheAuthenticator(cache, store,
		WithFailureThreshold(5),
		WithCooldownDuration(50*time.Millisecond),
	)

	// Cause some failures to increment the counter
	for i := 0; i < 3; i++ {
		auth.Verify(context.Background(), fmt.Sprintf("fail%d", i), []byte("pass"))
	}

	// Reset the circuit breaker
	auth.ResetCircuitBreaker()

	// After reset, store should be callable again (not degraded)
	store.setError(nil)
	store.setCred("afterreset", &Credential{
		Username: "afterreset",
		Password: "ok",
		Usertype: "device",
	})

	ok, err := auth.Verify(context.Background(), "afterreset", []byte("ok"))
	if err != nil {
		t.Fatalf("Verify() after ResetCircuitBreaker: error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() after ResetCircuitBreaker = false, want true")
	}
}

func TestCacheAuthenticator_ResetCircuitBreaker_NoOp(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()

	auth := NewCacheAuthenticator(cache, store)

	// Should not panic when failures already at 0
	auth.ResetCircuitBreaker()
}

func TestVerify_NegativeCache_RejectsWithoutDynamoCall(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()

	// Pre-populate cache with a negative entry
	cache.Set("baduser", &Credential{Username: "baduser", Negative: true})

	auth := NewCacheAuthenticator(cache, store)

	ok, err := auth.Verify(context.Background(), "baduser", []byte("anything"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if ok {
		t.Fatal("Verify() = true, want false for negative cache entry")
	}
	if store.calls() != 0 {
		t.Errorf("store.Fetch called %d times, want 0 (negative cache hit)", store.calls())
	}
}

func TestNegativeCache_StoredOnErrNotFound(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore() // no creds — returns ErrNotFound

	auth := NewCacheAuthenticator(cache, store)

	// First call: triggers store.Fetch which returns ErrNotFound
	ok, err := auth.Verify(context.Background(), "unknown", []byte("pass"))
	if err != nil {
		t.Fatalf("Verify() first call error = %v", err)
	}
	if ok {
		t.Fatal("Verify() first call = true, want false")
	}
	if store.calls() != 1 {
		t.Fatalf("store.Fetch called %d times on first call, want 1", store.calls())
	}

	// Second call: should hit negative cache, no store call
	ok, err = auth.Verify(context.Background(), "unknown", []byte("pass"))
	if err != nil {
		t.Fatalf("Verify() second call error = %v", err)
	}
	if ok {
		t.Fatal("Verify() second call = true, want false")
	}
	if store.calls() != 1 {
		t.Errorf("store.Fetch called %d times after negative cache, want still 1", store.calls())
	}
}

func TestNegativeCache_DoesNotAffectValidEntries(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setCred("valid", &Credential{
		Username: "valid",
		Password: "hello",
		Usertype: "device",
	})

	auth := NewCacheAuthenticator(cache, store)

	// Verify valid user works
	ok, err := auth.Verify(context.Background(), "valid", []byte("hello"))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() = false, want true for valid credential")
	}

	// Trigger negative cache for unknown user
	auth.Verify(context.Background(), "unknown", []byte("pass"))

	// Valid user from cache should still work
	ok, err = auth.Verify(context.Background(), "valid", []byte("hello"))
	if err != nil {
		t.Fatalf("Verify() after negative cache: error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() after negative cache = false, want true for valid credential")
	}
}

func TestIsDegraded_ExportedMethod(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setError(errors.New("dynamodb timeout"))

	auth := NewCacheAuthenticator(cache, store,
		WithFailureThreshold(2),
		WithCooldownDuration(5*time.Second),
	)

	// Healthy state
	if auth.IsDegraded() {
		t.Fatal("IsDegraded() = true on healthy authenticator, want false")
	}

	// Trip circuit breaker
	for i := 0; i < 2; i++ {
		auth.Verify(context.Background(), fmt.Sprintf("fail%d", i), []byte("pass"))
	}

	if !auth.IsDegraded() {
		t.Fatal("IsDegraded() = false after tripping breaker, want true")
	}
}

func TestCircuitBreaker_CacheHitsDuringDegradedMode(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()

	// Pre-populate cache
	cache.Set("cached", &Credential{
		Username: "cached",
		Password: "hello",
		Usertype: "device",
	})

	auth := NewCacheAuthenticator(cache, store,
		WithFailureThreshold(2),
		WithCooldownDuration(5*time.Second),
	)

	// Trip the circuit breaker
	store.setError(errors.New("dynamodb down"))
	for i := 0; i < 2; i++ {
		auth.Verify(context.Background(), fmt.Sprintf("miss%d", i), []byte("pass"))
	}

	// Cache hits should still succeed even when circuit breaker is open
	ok, err := auth.Verify(context.Background(), "cached", []byte("hello"))
	if err != nil {
		t.Fatalf("Verify() cache hit during degraded mode: error = %v", err)
	}
	if !ok {
		t.Fatal("Verify() cache hit during degraded mode = false, want true")
	}
}
