package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/whereiskurt/meshtk/internal/credcache"
)

// mockStore implements credcache.CredentialStore for testing.
type mockStore struct {
	mu    sync.Mutex
	creds map[string]*credcache.Credential
	err   error
}

func newMockStore() *mockStore {
	return &mockStore{
		creds: make(map[string]*credcache.Credential),
	}
}

func (m *mockStore) Fetch(_ context.Context, username string) (*credcache.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}
	cred, ok := m.creds[username]
	if !ok {
		return nil, credcache.ErrNotFound
	}
	return cred, nil
}

func (m *mockStore) setCred(username string, cred *credcache.Credential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creds[username] = cred
}

func (m *mockStore) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// newTestSetup creates a cache, mock store, authenticator, and admin server for tests.
func newTestSetup(t *testing.T) (*Server, *credcache.Cache, *mockStore) {
	t.Helper()
	cache, err := credcache.NewCache(60, 1)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	t.Cleanup(cache.Close)

	store := newMockStore()
	auth := credcache.NewCacheAuthenticator(cache, store)
	srv := NewServer(cache, store, auth, nil)
	return srv, cache, store
}

// envelope is used to decode JSON responses in tests.
type envelope struct {
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
	Timestamp string          `json:"timestamp"`
}

// evictData is the data payload for evict responses.
type evictData struct {
	Username string `json:"username"`
	Evicted  bool   `json:"evicted"`
}

// refreshData is the data payload for refresh responses.
type refreshData struct {
	Username  string `json:"username"`
	Refreshed bool   `json:"refreshed"`
}

// statsData is the data payload for stats responses.
type statsData struct {
	Entries   int     `json:"entries"`
	Hits      uint64  `json:"hits"`
	Misses    uint64  `json:"misses"`
	HitRate   float64 `json:"hit_rate"`
	Evictions uint64  `json:"evictions"`
}

func TestEvict_ExistingEntry(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	cache.Set("alice", &credcache.Credential{
		Username: "alice",
		Password: "68656c6c6f",
		Usertype: "device",
	})

	req := httptest.NewRequest(http.MethodDelete, "/cache/credentials/alice", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if env.Timestamp == "" {
		t.Error("timestamp should not be empty")
	}

	var data evictData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Username != "alice" {
		t.Errorf("username = %q, want alice", data.Username)
	}
	if !data.Evicted {
		t.Error("evicted = false, want true for existing entry")
	}

	// Verify actually removed from cache
	if _, ok := cache.Get("alice"); ok {
		t.Error("alice should be removed from cache after evict")
	}
}

func TestEvict_NonExistingEntry(t *testing.T) {
	srv, _, _ := newTestSetup(t)

	req := httptest.NewRequest(http.MethodDelete, "/cache/credentials/bob", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data evictData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Username != "bob" {
		t.Errorf("username = %q, want bob", data.Username)
	}
	if data.Evicted {
		t.Error("evicted = true, want false for non-existing entry")
	}
}

func TestRefresh_ExistingInDynamoDB(t *testing.T) {
	srv, cache, store := newTestSetup(t)

	store.setCred("alice", &credcache.Credential{
		Username: "alice",
		Password: "6e6577706173",
		Usertype: "device",
	})

	req := httptest.NewRequest(http.MethodPost, "/cache/credentials/alice/refresh", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if env.Timestamp == "" {
		t.Error("timestamp should not be empty")
	}

	var data refreshData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Username != "alice" {
		t.Errorf("username = %q, want alice", data.Username)
	}
	if !data.Refreshed {
		t.Error("refreshed = false, want true")
	}

	// Verify cache was updated
	cred, ok := cache.Get("alice")
	if !ok {
		t.Fatal("alice should be in cache after refresh")
	}
	if cred.Password != "6e6577706173" {
		t.Errorf("cached password = %q, want 6e6577706173", cred.Password)
	}
}

func TestRefresh_NotInDynamoDB(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	// Pre-populate cache -- should be evicted after 404 from DynamoDB
	cache.Set("ghost", &credcache.Credential{
		Username: "ghost",
		Password: "old",
		Usertype: "device",
	})

	req := httptest.NewRequest(http.MethodPost, "/cache/credentials/ghost/refresh", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if env.Error == "" {
		t.Error("error field should be non-empty for 404")
	}

	// Verify ghost was evicted from cache
	if _, ok := cache.Get("ghost"); ok {
		t.Error("ghost should be evicted from cache after 404")
	}
}

func TestRefresh_DynamoDBError(t *testing.T) {
	srv, _, store := newTestSetup(t)
	store.setError(errors.New("dynamodb connection timeout"))

	req := httptest.NewRequest(http.MethodPost, "/cache/credentials/alice/refresh", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if env.Error == "" {
		t.Error("error field should be non-empty for 502")
	}
}

func TestStats(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	// Generate some hits/misses
	cache.Set("alice", &credcache.Credential{Username: "alice", Password: "p", Usertype: "d"})
	cache.Get("alice")    // hit
	cache.Get("unknown")  // miss

	req := httptest.NewRequest(http.MethodGet, "/cache/stats", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if env.Timestamp == "" {
		t.Error("timestamp should not be empty")
	}

	var data statsData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	// Entries should be non-negative (Otter EstimatedSize is approximate)
	if data.Entries < 0 {
		t.Errorf("entries = %d, want non-negative", data.Entries)
	}
}

// listData is the data payload for list credentials responses.
type listData struct {
	Count   int         `json:"count"`
	Entries []listEntry `json:"entries"`
}

// listEntry is a single entry in the list credentials response.
type listEntry struct {
	Username     string `json:"username"`
	TTLRemaining int    `json:"ttl_remaining"`
	Negative     bool   `json:"negative"`
}

// flushData is the data payload for flush credentials responses.
type flushData struct {
	EvictedCount int  `json:"evicted_count"`
	StatsReset   bool `json:"stats_reset"`
}

// healthData is the data payload for health responses.
type healthData struct {
	Status       string `json:"status"`
	DynamoDB     string `json:"dynamodb"`
	CacheEntries int    `json:"cache_entries"`
}

func TestListCredentials_ReturnsEntries(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	cache.Set("alice", &credcache.Credential{Username: "alice", Password: "aaa", Usertype: "device"})
	cache.Set("bob", &credcache.Credential{Username: "bob", Password: "bbb", Usertype: "device"})

	req := httptest.NewRequest(http.MethodGet, "/cache/credentials", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data listData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Count != 2 {
		t.Errorf("count = %d, want 2", data.Count)
	}
	if len(data.Entries) != 2 {
		t.Fatalf("entries length = %d, want 2", len(data.Entries))
	}

	// Verify usernames are present (order may vary due to TTL sorting)
	usernames := map[string]bool{}
	for _, e := range data.Entries {
		usernames[e.Username] = true
		if e.TTLRemaining <= 0 {
			t.Errorf("ttl_remaining for %s = %d, want > 0", e.Username, e.TTLRemaining)
		}
	}
	if !usernames["alice"] || !usernames["bob"] {
		t.Errorf("expected alice and bob in entries, got %v", usernames)
	}
}

func TestListCredentials_NoPasswords(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	cache.Set("alice", &credcache.Credential{Username: "alice", Password: "secret", Usertype: "device"})

	req := httptest.NewRequest(http.MethodGet, "/cache/credentials", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Parse raw JSON to check no password or usertype fields leak
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data struct {
		Entries []map[string]interface{} `json:"entries"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}

	for _, entry := range data.Entries {
		if _, ok := entry["password"]; ok {
			t.Error("entry should not contain password field")
		}
		if _, ok := entry["usertype"]; ok {
			t.Error("entry should not contain usertype field")
		}
	}
}

func TestListCredentials_WithNegativeEntry(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	cache.Set("alice", &credcache.Credential{Username: "alice", Password: "aaa", Usertype: "device", Negative: false})
	cache.Set("ghost", &credcache.Credential{Username: "ghost", Negative: true})

	req := httptest.NewRequest(http.MethodGet, "/cache/credentials", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data listData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}

	negMap := map[string]bool{}
	for _, e := range data.Entries {
		negMap[e.Username] = e.Negative
	}

	if negMap["alice"] {
		t.Error("alice should not be negative")
	}
	if !negMap["ghost"] {
		t.Error("ghost should be negative")
	}
}

func TestListCredentials_SortedByTTL(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	// Set entries with different TTLs -- shorter TTL should come first
	cache.SetWithTTL("long", &credcache.Credential{Username: "long", Password: "p"}, 120*time.Second)
	cache.SetWithTTL("short", &credcache.Credential{Username: "short", Password: "p"}, 10*time.Second)
	cache.SetWithTTL("medium", &credcache.Credential{Username: "medium", Password: "p"}, 60*time.Second)

	req := httptest.NewRequest(http.MethodGet, "/cache/credentials", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data listData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}

	if len(data.Entries) != 3 {
		t.Fatalf("entries length = %d, want 3", len(data.Entries))
	}

	// Verify sorted ascending by TTL
	for i := 1; i < len(data.Entries); i++ {
		if data.Entries[i].TTLRemaining < data.Entries[i-1].TTLRemaining {
			t.Errorf("entries not sorted by TTL: [%d]=%d < [%d]=%d",
				i, data.Entries[i].TTLRemaining, i-1, data.Entries[i-1].TTLRemaining)
		}
	}
}

func TestListCredentials_Empty(t *testing.T) {
	srv, _, _ := newTestSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/cache/credentials", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data listData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}

	if data.Count != 0 {
		t.Errorf("count = %d, want 0", data.Count)
	}
	if data.Entries == nil {
		t.Error("entries should be empty array, not null")
	}
	if len(data.Entries) != 0 {
		t.Errorf("entries length = %d, want 0", len(data.Entries))
	}
}

func TestFlushCredentials_ClearsAll(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	cache.Set("alice", &credcache.Credential{Username: "alice", Password: "a"})
	cache.Set("bob", &credcache.Credential{Username: "bob", Password: "b"})
	cache.Set("charlie", &credcache.Credential{Username: "charlie", Password: "c"})

	req := httptest.NewRequest(http.MethodDelete, "/cache/credentials", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data flushData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}

	if data.EvictedCount < 1 {
		t.Errorf("evicted_count = %d, want >= 1", data.EvictedCount)
	}
	if data.StatsReset {
		t.Error("stats_reset = true, want false")
	}

	// Verify cache is empty
	if size := cache.Size(); size != 0 {
		t.Errorf("cache size after flush = %d, want 0", size)
	}
}

func TestFlushCredentials_EmptyCache(t *testing.T) {
	srv, _, _ := newTestSetup(t)

	req := httptest.NewRequest(http.MethodDelete, "/cache/credentials", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data flushData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}

	if data.EvictedCount != 0 {
		t.Errorf("evicted_count = %d, want 0", data.EvictedCount)
	}
}

func TestHealth_Healthy(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	cache.Set("alice", &credcache.Credential{Username: "alice", Password: "p"})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data healthData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}

	if data.Status != "healthy" {
		t.Errorf("status = %q, want healthy", data.Status)
	}
	if data.DynamoDB != "reachable" {
		t.Errorf("dynamodb = %q, want reachable", data.DynamoDB)
	}
	if data.CacheEntries < 0 {
		t.Errorf("cache_entries = %d, want >= 0", data.CacheEntries)
	}
}

func TestHealth_Degraded(t *testing.T) {
	cache, err := credcache.NewCache(60, 1)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	t.Cleanup(cache.Close)

	store := newMockStore()
	// Low threshold so we can trip it easily
	auth := credcache.NewCacheAuthenticator(cache, store, credcache.WithFailureThreshold(2))
	srv := NewServer(cache, store, auth, nil)

	// Trip the circuit breaker: cause 2 store failures
	store.setError(errors.New("dynamodb down"))
	auth.Verify(context.Background(), "user1", []byte("pass"))
	auth.Verify(context.Background(), "user2", []byte("pass"))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (health always returns 200)", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var data healthData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}

	if data.Status != "degraded" {
		t.Errorf("status = %q, want degraded", data.Status)
	}
	if data.DynamoDB != "unreachable" {
		t.Errorf("dynamodb = %q, want unreachable", data.DynamoDB)
	}
}

func TestRouteDisambiguation(t *testing.T) {
	srv, cache, _ := newTestSetup(t)

	// Test 1: Bulk flush (DELETE /cache/credentials)
	cache.Set("alice", &credcache.Credential{Username: "alice", Password: "a"})

	req := httptest.NewRequest(http.MethodDelete, "/cache/credentials", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("bulk flush: status = %d, want %d", w.Code, http.StatusOK)
	}

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("bulk flush: unmarshal response: %v", err)
	}

	// Should be flush response (evicted_count), not evict response (username)
	var flush flushData
	if err := json.Unmarshal(env.Data, &flush); err != nil {
		t.Fatalf("bulk flush: unmarshal data: %v", err)
	}

	// Test 2: Single evict (DELETE /cache/credentials/bob)
	cache.Set("bob", &credcache.Credential{Username: "bob", Password: "b"})

	req = httptest.NewRequest(http.MethodDelete, "/cache/credentials/bob", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("single evict: status = %d, want %d", w.Code, http.StatusOK)
	}

	var env2 envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env2); err != nil {
		t.Fatalf("single evict: unmarshal response: %v", err)
	}

	var evict evictData
	if err := json.Unmarshal(env2.Data, &evict); err != nil {
		t.Fatalf("single evict: unmarshal data: %v", err)
	}
	if evict.Username != "bob" {
		t.Errorf("single evict: username = %q, want bob", evict.Username)
	}
}

func TestContentTypeHeader(t *testing.T) {
	srv, _, _ := newTestSetup(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodDelete, "/cache/credentials/test"},
		{http.MethodPost, "/cache/credentials/test/refresh"},
		{http.MethodGet, "/cache/stats"},
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest(ep.method, ep.path, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s: Content-Type = %q, want application/json", ep.method, ep.path, ct)
		}
	}
}
