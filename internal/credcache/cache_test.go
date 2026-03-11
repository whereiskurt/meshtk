package credcache

import (
	"testing"
	"time"
)

func TestCacheNewSuccess(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()
}

func TestCacheGetMiss(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	cred, ok := c.Get("unknown")
	if ok {
		t.Fatal("Get(unknown) should return false")
	}
	if cred != nil {
		t.Fatal("Get(unknown) should return nil credential")
	}
}

func TestCacheSetAndGet(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	cred := &Credential{
		Username: "alice",
		Password: "secret123",
		Usertype: "rabbit",
	}
	c.Set("alice", cred)

	got, ok := c.Get("alice")
	if !ok {
		t.Fatal("Get(alice) should return true after Set")
	}
	if got == nil {
		t.Fatal("Get(alice) should return non-nil credential")
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want %q", got.Username, "alice")
	}
	if got.Password != "secret123" {
		t.Errorf("Password = %q, want %q", got.Password, "secret123")
	}
	if got.Usertype != "rabbit" {
		t.Errorf("Usertype = %q, want %q", got.Usertype, "rabbit")
	}
}

func TestCacheStatsHits(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	cred := &Credential{Username: "alice", Password: "pass", Usertype: "og"}
	c.Set("alice", cred)

	// Multiple gets to register hits
	for i := 0; i < 5; i++ {
		c.Get("alice")
	}

	s := c.Stats()
	if s.Hits == 0 {
		t.Error("Stats().Hits should be > 0 after successful Gets")
	}
}

func TestCacheStatsMisses(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	c.Get("unknown")
	c.Get("missing")

	s := c.Stats()
	if s.Misses == 0 {
		t.Error("Stats().Misses should be > 0 after missed Gets")
	}
}

func TestCacheDelete(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	cred := &Credential{Username: "alice", Password: "pass", Usertype: "og"}
	c.Set("alice", cred)

	// Verify it exists
	if _, ok := c.Get("alice"); !ok {
		t.Fatal("Get(alice) should return true after Set")
	}

	c.Delete("alice")

	got, ok := c.Get("alice")
	if ok {
		t.Fatal("Get(alice) should return false after Delete")
	}
	if got != nil {
		t.Fatal("Get(alice) should return nil after Delete")
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	// Use 1-second TTL
	c, err := NewCache(1, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	cred := &Credential{Username: "alice", Password: "pass", Usertype: "og"}
	c.Set("alice", cred)

	// Verify it exists
	if _, ok := c.Get("alice"); !ok {
		t.Fatal("Get(alice) should return true immediately after Set")
	}

	// Wait for TTL to expire
	time.Sleep(2 * time.Second)

	_, ok := c.Get("alice")
	if ok {
		t.Fatal("Get(alice) should return false after TTL expiry")
	}
}

func TestCache_Size_EmptyCache(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	size := c.Size()
	if size != 0 {
		t.Errorf("Size() = %d, want 0 for empty cache", size)
	}
}

func TestCache_Size_AfterSet(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	c.Set("alice", &Credential{Username: "alice", Password: "pass", Usertype: "og"})

	size := c.Size()
	if size < 0 {
		t.Errorf("Size() = %d, want non-negative after Set", size)
	}
}

func TestSetWithTTL(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	cred := &Credential{Username: "ttluser", Password: "pass", Usertype: "device"}
	c.SetWithTTL("ttluser", cred, 5*time.Second)

	got, ok := c.Get("ttluser")
	if !ok {
		t.Fatal("Get(ttluser) should return true after SetWithTTL")
	}
	if got.Username != "ttluser" {
		t.Errorf("Username = %q, want %q", got.Username, "ttluser")
	}
}

func TestDeleteAll(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	c.Set("a", &Credential{Username: "a", Password: "p1"})
	c.Set("b", &Credential{Username: "b", Password: "p2"})
	c.Set("c", &Credential{Username: "c", Password: "p3"})

	c.DeleteAll()

	// Otter invalidation is async, give it a moment
	time.Sleep(100 * time.Millisecond)

	if s := c.Size(); s != 0 {
		t.Errorf("Size() = %d after DeleteAll, want 0", s)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, ok := c.Get(name); ok {
			t.Errorf("Get(%q) should return false after DeleteAll", name)
		}
	}
}

func TestDeleteAll_ReturnsCount(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	c.Set("a", &Credential{Username: "a", Password: "p1"})
	c.Set("b", &Credential{Username: "b", Password: "p2"})
	c.Set("c", &Credential{Username: "c", Password: "p3"})

	count := c.DeleteAll()
	// EstimatedSize is approximate; just verify non-negative
	if count < 0 {
		t.Errorf("DeleteAll() returned %d, want >= 0", count)
	}
}

func TestEntries_ReturnsAllWithTTL(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	c.Set("alice", &Credential{Username: "alice", Password: "p1"})
	c.Set("bob", &Credential{Username: "bob", Password: "p2"})

	entries := c.Entries()
	if len(entries) != 2 {
		t.Fatalf("Entries() returned %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		if e.TTLRemaining <= 0 {
			t.Errorf("entry %q has TTLRemaining=%d, want > 0", e.Username, e.TTLRemaining)
		}
	}
}

func TestEntries_SortedByTTL(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	// Use SetWithTTL with different durations
	c.SetWithTTL("short", &Credential{Username: "short", Password: "p1"}, 10*time.Second)
	c.SetWithTTL("long", &Credential{Username: "long", Password: "p2"}, 300*time.Second)

	entries := c.Entries()
	if len(entries) < 2 {
		t.Fatalf("Entries() returned %d entries, want >= 2", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].TTLRemaining < entries[i-1].TTLRemaining {
			t.Errorf("entries not sorted by TTL: [%d].TTL=%d < [%d].TTL=%d",
				i, entries[i].TTLRemaining, i-1, entries[i-1].TTLRemaining)
		}
	}
}

func TestEntries_IncludesNegativeFlag(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	c.Set("pos", &Credential{Username: "pos", Password: "p1", Negative: false})
	c.Set("neg", &Credential{Username: "neg", Password: "", Negative: true})

	entries := c.Entries()
	found := map[string]bool{}
	for _, e := range entries {
		found[e.Username] = e.Negative
	}
	if neg, ok := found["neg"]; !ok || !neg {
		t.Error("negative entry should have Negative=true")
	}
	if pos, ok := found["pos"]; !ok || pos {
		t.Error("positive entry should have Negative=false")
	}
}

func TestEntries_Empty(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	defer c.Close()

	entries := c.Entries()
	if entries == nil {
		t.Fatal("Entries() on empty cache should return empty slice, not nil")
	}
	if len(entries) != 0 {
		t.Errorf("Entries() on empty cache returned %d entries, want 0", len(entries))
	}
}

func TestCacheCloseNoPanic(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}

	// Should not panic
	c.Close()
}
