package keycache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockStore implements KeyStore for testing, keyed by uint32 nodeNum.
type mockStore struct {
	mu        sync.Mutex
	keys      map[uint32]*Key
	err       error
	callCount atomic.Int64
	// delay lets tests widen the singleflight window deterministically.
	delay time.Duration
}

func newMockStore() *mockStore {
	return &mockStore{keys: make(map[uint32]*Key)}
}

func (m *mockStore) Fetch(ctx context.Context, nodeNum uint32) (*Key, error) {
	m.callCount.Add(1)
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}
	key, ok := m.keys[nodeNum]
	if !ok {
		return nil, ErrNotFound
	}
	return key, nil
}

func (m *mockStore) setKey(nodeNum uint32, key *Key) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[nodeNum] = key
}

func (m *mockStore) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *mockStore) calls() int64 { return m.callCount.Load() }

func newTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := NewCache(90, 1) // 90s TTL, 1MB
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

const sampleHex = "0x433d1cec00112233445566778899aabbccddeeff00112233445566778899aabb"

func TestResolve_CacheHit_NoStoreCall(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()

	nodeNum := uint32(0x433d1cec)
	cache.Set(NodeIDFromNum(nodeNum), &Key{NodeID: NodeIDFromNum(nodeNum), NodeNum: nodeNum, PubKeyHex: sampleHex})

	r := NewKeyResolver(cache, store)

	hex, ok, err := r.Resolve(context.Background(), nodeNum)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !ok {
		t.Fatal("Resolve() ok = false, want true for cached key")
	}
	if hex != sampleHex {
		t.Errorf("Resolve() hex = %q, want %q", hex, sampleHex)
	}
	if store.calls() != 0 {
		t.Errorf("store.Fetch called %d times, want 0 (cache hit)", store.calls())
	}
}

func TestResolve_CacheMiss_FetchAndPopulate(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	nodeNum := uint32(0x433d1cec)
	store.setKey(nodeNum, &Key{PubKeyHex: sampleHex})

	r := NewKeyResolver(cache, store)

	hex, ok, err := r.Resolve(context.Background(), nodeNum)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !ok || hex != sampleHex {
		t.Fatalf("Resolve() = (%q, %v), want (%q, true)", hex, ok, sampleHex)
	}
	if store.calls() != 1 {
		t.Errorf("store.Fetch called %d times, want 1", store.calls())
	}

	// Cache populated: second call hits cache, no further store call.
	if _, ok := cache.Get(NodeIDFromNum(nodeNum)); !ok {
		t.Fatal("cache should contain node after fetch")
	}
	_, _, _ = r.Resolve(context.Background(), nodeNum)
	if store.calls() != 1 {
		t.Errorf("store.Fetch called %d times after cache populate, want still 1", store.calls())
	}
}

func TestResolve_EmptyNodeNum(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	r := NewKeyResolver(cache, store)

	hex, ok, err := r.Resolve(context.Background(), 0)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if ok || hex != "" {
		t.Fatalf("Resolve(0) = (%q, %v), want (\"\", false)", hex, ok)
	}
	if store.calls() != 0 {
		t.Errorf("store.Fetch called %d times for nodeNum 0, want 0", store.calls())
	}
}

func TestResolve_NegativeCache_UnknownSenderBoundsReads(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore() // no keys → ErrNotFound
	r := NewKeyResolver(cache, store)

	nodeNum := uint32(0xdeadbeef)

	// First lookup: one store call, negative-cached.
	hex, ok, err := r.Resolve(context.Background(), nodeNum)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if ok || hex != "" {
		t.Fatalf("Resolve() = (%q, %v), want miss", hex, ok)
	}
	if store.calls() != 1 {
		t.Fatalf("store.Fetch called %d times on first miss, want 1", store.calls())
	}

	// Subsequent lookups served from negative entry — no further store calls.
	for i := 0; i < 5; i++ {
		_, ok, err := r.Resolve(context.Background(), nodeNum)
		if err != nil || ok {
			t.Fatalf("Resolve() repeat = (ok=%v, err=%v), want miss", ok, err)
		}
	}
	if store.calls() != 1 {
		t.Errorf("store.Fetch called %d times after negative cache, want still 1", store.calls())
	}
}

func TestResolve_NegativeCache_PreExisting_NoStoreCall(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	nodeNum := uint32(0x00000abc)
	cache.Set(NodeIDFromNum(nodeNum), &Key{NodeID: NodeIDFromNum(nodeNum), NodeNum: nodeNum, Negative: true})

	r := NewKeyResolver(cache, store)

	_, ok, err := r.Resolve(context.Background(), nodeNum)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if ok {
		t.Fatal("Resolve() ok = true, want false for negative cache entry")
	}
	if store.calls() != 0 {
		t.Errorf("store.Fetch called %d times, want 0 (negative cache hit)", store.calls())
	}
}

func TestResolve_Singleflight_CollapsesConcurrentMisses(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.delay = 50 * time.Millisecond // widen the singleflight window
	nodeNum := uint32(0x433d1cec)
	store.setKey(nodeNum, &Key{PubKeyHex: sampleHex})

	r := NewKeyResolver(cache, store)

	const concurrency = 20
	var wg sync.WaitGroup
	results := make([]bool, concurrency)
	errs := make([]error, concurrency)

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			_, ok, err := r.Resolve(context.Background(), nodeNum)
			results[idx] = ok
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Resolve() error = %v", i, err)
		}
	}
	for i, ok := range results {
		if !ok {
			t.Errorf("goroutine %d: Resolve() ok = false, want true", i)
		}
	}
	if calls := store.calls(); calls != 1 {
		t.Errorf("store.Fetch called %d times, want exactly 1 (singleflight collapse)", calls)
	}
}

func TestResolve_CircuitBreaker_OpensAndShortCircuits(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setError(errors.New("dynamodb timeout"))

	r := NewKeyResolver(cache, store,
		WithFailureThreshold(2),
		WithCooldownDuration(5*time.Second),
	)

	// First 2 misses attempt the store and fail.
	for i := 0; i < 2; i++ {
		if _, _, err := r.Resolve(context.Background(), uint32(0x1000+i)); err == nil {
			t.Fatalf("call %d: Resolve() error = nil, want error", i)
		}
	}
	if store.calls() != 2 {
		t.Fatalf("store.Fetch called %d times before trip, want 2", store.calls())
	}
	if !r.IsDegraded() {
		t.Fatal("IsDegraded() = false after threshold failures, want true")
	}

	// Third call short-circuits without hitting the store.
	if _, _, err := r.Resolve(context.Background(), uint32(0x2000)); err == nil {
		t.Fatal("Resolve() error = nil, want circuit breaker error")
	}
	if store.calls() != 2 {
		t.Errorf("store.Fetch called %d times after trip, want still 2", store.calls())
	}
}

func TestResolve_CircuitBreaker_RecoversAfterCooldown(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setError(errors.New("dynamodb timeout"))

	r := NewKeyResolver(cache, store,
		WithFailureThreshold(2),
		WithCooldownDuration(50*time.Millisecond),
	)

	for i := 0; i < 2; i++ {
		_, _, _ = r.Resolve(context.Background(), uint32(0x3000+i))
	}
	time.Sleep(100 * time.Millisecond)

	store.setError(nil)
	nodeNum := uint32(0x433d1cec)
	store.setKey(nodeNum, &Key{PubKeyHex: sampleHex})

	hex, ok, err := r.Resolve(context.Background(), nodeNum)
	if err != nil {
		t.Fatalf("Resolve() after recovery error = %v", err)
	}
	if !ok || hex != sampleHex {
		t.Fatalf("Resolve() after recovery = (%q, %v), want (%q, true)", hex, ok, sampleHex)
	}
}

func TestResolve_CacheHitsDuringDegradedMode(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	nodeNum := uint32(0x433d1cec)
	cache.Set(NodeIDFromNum(nodeNum), &Key{NodeID: NodeIDFromNum(nodeNum), NodeNum: nodeNum, PubKeyHex: sampleHex})

	r := NewKeyResolver(cache, store,
		WithFailureThreshold(2),
		WithCooldownDuration(5*time.Second),
	)

	store.setError(errors.New("dynamodb down"))
	for i := 0; i < 2; i++ {
		_, _, _ = r.Resolve(context.Background(), uint32(0x4000+i))
	}
	if !r.IsDegraded() {
		t.Fatal("IsDegraded() = false, want true after tripping")
	}

	// Cache hit still succeeds while the breaker is open.
	hex, ok, err := r.Resolve(context.Background(), nodeNum)
	if err != nil {
		t.Fatalf("Resolve() cache hit during degraded mode error = %v", err)
	}
	if !ok || hex != sampleHex {
		t.Fatalf("Resolve() cache hit during degraded = (%q, %v), want (%q, true)", hex, ok, sampleHex)
	}
}

func TestResolve_ResetCircuitBreaker(t *testing.T) {
	cache := newTestCache(t)
	store := newMockStore()
	store.setError(errors.New("dynamodb timeout"))

	r := NewKeyResolver(cache, store,
		WithFailureThreshold(2),
		WithCooldownDuration(5*time.Second),
	)

	for i := 0; i < 2; i++ {
		_, _, _ = r.Resolve(context.Background(), uint32(0x5000+i))
	}
	if !r.IsDegraded() {
		t.Fatal("IsDegraded() = false, want true")
	}

	r.ResetCircuitBreaker()
	if r.IsDegraded() {
		t.Fatal("IsDegraded() = true after reset, want false")
	}
}
