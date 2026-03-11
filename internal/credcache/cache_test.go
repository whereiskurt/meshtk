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

func TestCacheCloseNoPanic(t *testing.T) {
	c, err := NewCache(900, 64)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}

	// Should not panic
	c.Close()
}
