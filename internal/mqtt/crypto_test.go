package mqtt

import (
	"bytes"
	"context"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/whereiskurt/meshtk/internal/keycache"
)

// Two distinct 32-byte X25519-shaped hex keys. realKey is the authoritative
// MeshRadio key (DDB); bogusKey stands in for a NODEINFO-injected nodes.json key
// — the exact hotfix exploit (project_ghost_chatbot_reply_debug): a poisoned feed
// key must never change decrypt behavior once fallback=none.
const (
	realKey  = "0x433d1cec00112233445566778899aabbccddeeff00112233445566778899aabb"
	bogusKey = "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
)

// fakeKeyStore implements keycache.KeyStore over an in-memory map (no DDB, no
// network). Absence yields ErrNotFound, exactly like a real MeshRadio GetItem miss.
type fakeKeyStore struct {
	keys map[uint32]*keycache.Key
}

func (f *fakeKeyStore) Fetch(_ context.Context, nodeNum uint32) (*keycache.Key, error) {
	if k, ok := f.keys[nodeNum]; ok {
		return k, nil
	}
	return nil, keycache.ErrNotFound
}

// feedSpy records nodes.json fallback calls and returns a fixed key. It proves
// whether the bogus feed path was consulted at all.
type feedSpy struct {
	calls  int
	retKey string
}

func (s *feedSpy) fetch(nodeNum uint32) (string, error) {
	s.calls++
	return s.retKey, nil
}

// newTestClient builds an MqttClient wired with a real keycache.KeyResolver over
// a fake store, a buffered logger (for enrollment-coverage log assertions), and a
// feed spy standing in for the nodes.json fallback branch. storeKeys are the
// authoritative DDB keys; feedKey is what the (poisonable) feed would return.
func newTestClient(t *testing.T, fallback string, storeKeys map[uint32]*keycache.Key, feedKey string) (*MqttClient, *bytes.Buffer, *feedSpy) {
	t.Helper()

	cache, err := keycache.NewCache(90, 1)
	if err != nil {
		t.Fatalf("NewCache() error = %v", err)
	}
	t.Cleanup(cache.Close)

	store := &fakeKeyStore{keys: storeKeys}
	resolver := keycache.NewKeyResolver(cache, store)

	buf := &bytes.Buffer{}
	logger := log.New()
	logger.SetOutput(buf)
	logger.SetLevel(log.InfoLevel)

	spy := &feedSpy{retKey: feedKey}

	c := &MqttClient{
		log:         logger,
		keyResolver: resolver,
		keyFallback: fallback,
		nodesFeedFn: spy.fetch,
	}
	return c, buf, spy
}

// Case A: an authoritative keycache hit returns the DDB key and the nodes.json
// feed is never consulted — even under the bring-up fallback=nodes.json.
func TestResolveSenderPubKey_CacheHit_NodesJsonNeverConsulted(t *testing.T) {
	const node = uint32(0x433d1cec)
	keys := map[uint32]*keycache.Key{node: {PubKeyHex: realKey}}
	c, _, spy := newTestClient(t, "nodes.json", keys, bogusKey)

	got, err := c.resolveSenderPubKey(node)
	if err != nil {
		t.Fatalf("resolveSenderPubKey() error = %v", err)
	}
	if got != realKey {
		t.Errorf("resolveSenderPubKey() = %q, want authoritative %q", got, realKey)
	}
	if spy.calls != 0 {
		t.Errorf("nodes.json feed consulted %d times on a keycache hit, want 0", spy.calls)
	}
}

// Case B (SECURITY REGRESSION, Success Criterion #3): under fallback=none a bogus
// key present only in the nodes.json feed must NEVER override or substitute for
// the authoritative decision.
//   - B1: keycache HAS the node → returns the real key, feed never consulted.
//   - B2: keycache is MISSING the node → returns an error (→ NACK), the bogus feed
//     key is never returned. A NODEINFO injection cannot change decrypt behavior.
func TestResolveSenderPubKey_Regression_BogusFeedKeyIgnoredUnderFallbackNone(t *testing.T) {
	const node = uint32(0x433d1cec)

	t.Run("keycache_hit_wins_over_bogus_feed", func(t *testing.T) {
		keys := map[uint32]*keycache.Key{node: {PubKeyHex: realKey}}
		c, _, spy := newTestClient(t, "none", keys, bogusKey)

		got, err := c.resolveSenderPubKey(node)
		if err != nil {
			t.Fatalf("resolveSenderPubKey() error = %v", err)
		}
		if got != realKey {
			t.Fatalf("resolveSenderPubKey() = %q, want authoritative %q (NOT the bogus feed key)", got, realKey)
		}
		if got == bogusKey {
			t.Fatal("SECURITY: returned the poisoned feed key")
		}
		if spy.calls != 0 {
			t.Errorf("feed consulted %d times under fallback=none, want 0", spy.calls)
		}
	})

	t.Run("keycache_miss_nacks_never_uses_bogus_feed", func(t *testing.T) {
		c, _, spy := newTestClient(t, "none", map[uint32]*keycache.Key{}, bogusKey)

		got, err := c.resolveSenderPubKey(node)
		if err == nil {
			t.Fatalf("resolveSenderPubKey() error = nil, want miss error → NACK (got key %q)", got)
		}
		if got == bogusKey {
			t.Fatal("SECURITY: returned the poisoned feed key on a keycache miss under fallback=none")
		}
		if got != "" {
			t.Errorf("resolveSenderPubKey() = %q, want empty on miss", got)
		}
		if spy.calls != 0 {
			t.Errorf("feed consulted %d times under fallback=none, want 0", spy.calls)
		}
	})
}

// Case C: under fallback=nodes.json a keycache miss falls through to the feed and
// logs the fallback for enrollment-coverage measurement.
func TestResolveSenderPubKey_FallbackNodesJson_MissFallsThroughAndLogs(t *testing.T) {
	const node = uint32(0x00abcdef)
	c, buf, spy := newTestClient(t, "nodes.json", map[uint32]*keycache.Key{}, realKey)

	got, err := c.resolveSenderPubKey(node)
	if err != nil {
		t.Fatalf("resolveSenderPubKey() error = %v", err)
	}
	if got != realKey {
		t.Errorf("resolveSenderPubKey() = %q, want feed key %q", got, realKey)
	}
	if spy.calls != 1 {
		t.Errorf("feed consulted %d times, want exactly 1 (fallthrough)", spy.calls)
	}
	logs := buf.String()
	if !strings.Contains(logs, "nodes.json fallback used") || !strings.Contains(logs, "enrollment-coverage") {
		t.Errorf("missing enrollment-coverage fallback log; got:\n%s", logs)
	}
}

// Case D: under fallback=none a keycache miss returns an error (→ existing
// nackHandler), never consults the feed, and logs the miss.
func TestResolveSenderPubKey_FallbackNone_MissErrorsAndLogs(t *testing.T) {
	const node = uint32(0x00abcdef)
	c, buf, spy := newTestClient(t, "none", map[uint32]*keycache.Key{}, bogusKey)

	got, err := c.resolveSenderPubKey(node)
	if err == nil {
		t.Fatalf("resolveSenderPubKey() error = nil, want miss error (got %q)", got)
	}
	if spy.calls != 0 {
		t.Errorf("feed consulted %d times under fallback=none, want 0", spy.calls)
	}
	logs := buf.String()
	if !strings.Contains(logs, "fallback=none") || !strings.Contains(logs, "enrollment-coverage") {
		t.Errorf("missing enrollment-coverage NACK log; got:\n%s", logs)
	}
}

// A nil resolver (e.g. the nodeinfo utility command, or a resolver that failed to
// build) preserves the legacy nodes.json feed path.
func TestResolveSenderPubKey_NilResolver_UsesFeed(t *testing.T) {
	spy := &feedSpy{retKey: realKey}
	buf := &bytes.Buffer{}
	logger := log.New()
	logger.SetOutput(buf)

	c := &MqttClient{log: logger, nodesFeedFn: spy.fetch}

	got, err := c.resolveSenderPubKey(uint32(0x00abcdef))
	if err != nil {
		t.Fatalf("resolveSenderPubKey() error = %v", err)
	}
	if got != realKey {
		t.Errorf("resolveSenderPubKey() = %q, want feed key %q", got, realKey)
	}
	if spy.calls != 1 {
		t.Errorf("feed consulted %d times, want 1 (legacy path)", spy.calls)
	}
}
