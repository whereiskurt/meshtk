package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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
