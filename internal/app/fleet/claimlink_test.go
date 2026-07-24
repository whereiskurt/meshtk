package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/whereiskurt/meshtk/pkg/config"
)

func TestMintClaimURLErrorsWhenUnconfigured(t *testing.T) {
	t.Setenv("MESHTK_RUN_INTERNAL_URL", "")
	t.Setenv("MESHTK_INTERNAL_SECRET", "")
	if _, err := mintClaimURL(context.Background(), "ghost.goldstein"); err == nil {
		t.Fatal("unset env must error (caller falls back to static reveal)")
	}
}

func TestMintClaimURLPostsGhostWithSecretAndReturnsURL(t *testing.T) {
	var gotSecret, gotGhost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get("x-internal-secret")
		var body struct {
			Ghost string `json:"ghost"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotGhost = body.Ghost
		w.Write([]byte(`{"nonce":"n1","url":"https://run.defcon.run/use1/ctf/claim?nonce=n1"}`))
	}))
	defer srv.Close()
	t.Setenv("MESHTK_RUN_INTERNAL_URL", srv.URL)
	t.Setenv("MESHTK_INTERNAL_SECRET", "s3cret")

	url, err := mintClaimURL(context.Background(), "ghost.goldstein")
	if err != nil {
		t.Fatalf("mint failed: %v", err)
	}
	if url != "https://run.defcon.run/use1/ctf/claim?nonce=n1" {
		t.Errorf("wrong url: %q", url)
	}
	if gotSecret != "s3cret" || gotGhost != "ghost.goldstein" {
		t.Errorf("request wrong: secret=%q ghost=%q", gotSecret, gotGhost)
	}
}

func TestMintClaimURLErrorsOnNon200AndEmptyURL(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"422":       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(422) },
		"empty-url": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"nonce":"n"}`)) },
		"bad-json":  func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`nope`)) },
	} {
		srv := httptest.NewServer(handler)
		t.Setenv("MESHTK_RUN_INTERNAL_URL", srv.URL)
		t.Setenv("MESHTK_INTERNAL_SECRET", "s")
		if _, err := mintClaimURL(context.Background(), "ghost.x"); err == nil {
			t.Errorf("%s: want error", name)
		}
		srv.Close()
	}
}

// One mint per radio per unlock: the second trigger returns the cached URL off
// the OTPUnlock record without a second HTTP call, and the cache is per-radio.
func TestGetOrMintRevealURLMintsOncePerUnlock(t *testing.T) {
	var mints int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		w.Write([]byte(`{"nonce":"n","url":"https://run/claim?nonce=n"}`))
	}))
	defer srv.Close()
	t.Setenv("MESHTK_RUN_INTERNAL_URL", srv.URL)
	t.Setenv("MESHTK_INTERNAL_SECRET", "s")

	n := &FleetCmd{
		Config:       &config.Config{Fleet: []config.Fleet{{Id: "ghost.goldstein"}}},
		OTPUnlocks:   []map[uint32]*OTPUnlock{{42: {UnlockTimestamp: time.Now()}}},
		OTPUnlockMux: []sync.RWMutex{{}},
	}

	first, err := n.getOrMintRevealURL(context.Background(), 0, 42)
	if err != nil {
		t.Fatalf("first mint failed: %v", err)
	}
	second, err := n.getOrMintRevealURL(context.Background(), 0, 42)
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if first != second {
		t.Errorf("re-trigger got a DIFFERENT link: %q vs %q", first, second)
	}
	if mints != 1 {
		t.Errorf("minted %d times, want 1 (no token farming)", mints)
	}
	if n.OTPUnlocks[0][42].RevealURL != first {
		t.Error("URL not cached on the unlock record")
	}
}

func TestGetOrMintRevealURLErrorsWhenMintDown(t *testing.T) {
	t.Setenv("MESHTK_RUN_INTERNAL_URL", "http://127.0.0.1:1") // refused
	t.Setenv("MESHTK_INTERNAL_SECRET", "s")
	n := &FleetCmd{
		Config:       &config.Config{Fleet: []config.Fleet{{Id: "ghost.goldstein"}}},
		OTPUnlocks:   []map[uint32]*OTPUnlock{{42: {UnlockTimestamp: time.Now()}}},
		OTPUnlockMux: []sync.RWMutex{{}},
	}
	if _, err := n.getOrMintRevealURL(context.Background(), 0, 42); err == nil {
		t.Fatal("mint transport failure must surface as error (fallback path)")
	}
	if n.OTPUnlocks[0][42].RevealURL != "" {
		t.Error("failed mint must not cache anything")
	}
}
