package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

// The challenge shape names the Ctf row directly instead of a ghost fleet id:
// ricky's award has no ghost unlock session to hang off, and run.human parks
// the challenge row's own stored answerHash so no raw flag code need exist.
func TestMintClaimURLForChallengePostsChallengeWithSecret(t *testing.T) {
	var gotSecret, gotChallenge, gotGhost, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get("x-internal-secret")
		gotType = r.Header.Get("content-type")
		var body struct {
			Challenge string `json:"challenge"`
			Ghost     string `json:"ghost"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotChallenge, gotGhost = body.Challenge, body.Ghost
		w.Write([]byte(`{"nonce":"n1","url":"https://q.defcon.run/a/abc123def456"}`))
	}))
	defer srv.Close()
	t.Setenv("MESHTK_RUN_INTERNAL_URL", srv.URL)
	t.Setenv("MESHTK_INTERNAL_SECRET", "s3cret")

	url, err := mintClaimURLForChallenge(context.Background(), "ricky")
	if err != nil {
		t.Fatalf("mint failed: %v", err)
	}
	if url != "https://q.defcon.run/a/abc123def456" {
		t.Errorf("wrong url: %q", url)
	}
	if gotChallenge != "ricky" {
		t.Errorf("challenge = %q, want %q", gotChallenge, "ricky")
	}
	if gotGhost != "" {
		t.Errorf("challenge mint must not send a ghost key, sent %q", gotGhost)
	}
	if gotSecret != "s3cret" {
		t.Errorf("internal-secret header = %q", gotSecret)
	}
	if gotType != "application/json" {
		t.Errorf("content-type = %q", gotType)
	}
}

func TestMintClaimURLForChallengeErrorsOnBadResponses(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"422":       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(422) },
		"500":       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"empty-url": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"nonce":"n"}`)) },
		"bad-json":  func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`nope`)) },
	} {
		srv := httptest.NewServer(handler)
		t.Setenv("MESHTK_RUN_INTERNAL_URL", srv.URL)
		t.Setenv("MESHTK_INTERNAL_SECRET", "s")
		if _, err := mintClaimURLForChallenge(context.Background(), "ricky"); err == nil {
			t.Errorf("%s: want error", name)
		}
		srv.Close()
	}
}

// Unconfigured env must short-circuit BEFORE the request: a half-configured
// container would otherwise POST an unauthenticated mint at the real endpoint.
func TestMintClaimURLForChallengeErrorsWhenUnconfiguredWithoutRequesting(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"url":"https://q.defcon.run/a/leak"}`))
	}))
	defer srv.Close()

	for name, env := range map[string][2]string{
		"no-base":   {"", "s"},
		"no-secret": {srv.URL, ""},
		"neither":   {"", ""},
	} {
		t.Setenv("MESHTK_RUN_INTERNAL_URL", env[0])
		t.Setenv("MESHTK_INTERNAL_SECRET", env[1])
		if _, err := mintClaimURLForChallenge(context.Background(), "ricky"); err == nil {
			t.Errorf("%s: unset env must error", name)
		}
	}
	if hits != 0 {
		t.Errorf("unconfigured mint made %d request(s); must not touch the network", hits)
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

// The award headline may be the ONLY message a requester gets (mint down and
// no fallback configured), and there is no length check anywhere in the send
// path — an over-length message is a silent drop, not a rejection.
func TestAwardHeadlineFitsTheChatHardLimit(t *testing.T) {
	if n := len(awardHeadline); n >= chatHardLimit {
		t.Errorf("awardHeadline is %d bytes, must be under chatHardLimit (%d)", n, chatHardLimit)
	}
	if awardHeadline == "" {
		t.Error("awardHeadline must not be empty; a silent song end is the bug this replaces")
	}
}

func TestRickyChallengeDefaultsOverridesAndDisables(t *testing.T) {
	// t.Setenv registers the restore; unsetting afterwards still restores.
	t.Setenv("MESHTK_RICKY_CHALLENGE", "placeholder")
	os.Unsetenv("MESHTK_RICKY_CHALLENGE")
	if got := rickyChallenge(); got != "ricky" {
		t.Errorf("unset challenge = %q, want %q", got, "ricky")
	}

	t.Setenv("MESHTK_RICKY_CHALLENGE", "encore")
	if got := rickyChallenge(); got != "encore" {
		t.Errorf("override = %q, want %q", got, "encore")
	}

	// Explicitly empty is the kill switch: a lyric fleet with no flag awards nothing.
	t.Setenv("MESHTK_RICKY_CHALLENGE", "")
	if got := rickyChallenge(); got != "" {
		t.Errorf("explicit empty = %q, want %q (award disabled)", got, "")
	}
}

func lyricsFleet() *FleetCmd {
	return &FleetCmd{
		LyricsResponded: []map[uint32]*lyricsSession{{}},
		LyricsRespMux:   []sync.RWMutex{{}},
	}
}

// At most one mint per radio per cooldown window: the URL caches on the lyric
// session, so a second resolution inside the same showtime makes no HTTP call.
func TestAwardClaimURLMintsOncePerSession(t *testing.T) {
	var mints int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		w.Write([]byte(`{"nonce":"n","url":"https://q.defcon.run/a/zzz111yyy222"}`))
	}))
	defer srv.Close()
	t.Setenv("MESHTK_RUN_INTERNAL_URL", srv.URL)
	t.Setenv("MESHTK_INTERNAL_SECRET", "s")
	t.Setenv("MESHTK_RICKY_FALLBACK_URL", "")

	n := lyricsFleet()
	session := &lyricsSession{at: time.Now()}
	n.LyricsResponded[0][42] = session

	first := n.awardClaimURL(context.Background(), 0, session, "ricky")
	second := n.awardClaimURL(context.Background(), 0, session, "ricky")
	if first != "https://q.defcon.run/a/zzz111yyy222" {
		t.Errorf("wrong url: %q", first)
	}
	if first != second {
		t.Errorf("encore got a DIFFERENT link: %q vs %q", first, second)
	}
	if mints != 1 {
		t.Errorf("minted %d times, want 1 (no token farming)", mints)
	}
	if session.url != first {
		t.Errorf("url not cached on the lyric session: %q", session.url)
	}
}

// Mint failure must never leave a player empty-handed when a fallback exists,
// and must never cache the fallback (a later showtime retries the real mint).
func TestAwardClaimURLFallsBackWhenMintFails(t *testing.T) {
	t.Setenv("MESHTK_RUN_INTERNAL_URL", "http://127.0.0.1:1") // refused
	t.Setenv("MESHTK_INTERNAL_SECRET", "s")
	t.Setenv("MESHTK_RICKY_FALLBACK_URL", "https://q.defcon.run/a/fallback1234")

	n := lyricsFleet()
	session := &lyricsSession{at: time.Now()}
	n.LyricsResponded[0][42] = session

	got := n.awardClaimURL(context.Background(), 0, session, "ricky")
	if got != "https://q.defcon.run/a/fallback1234" {
		t.Errorf("fallback url = %q", got)
	}
	if session.url != "" {
		t.Errorf("fallback must not be cached on the session, got %q", session.url)
	}
}

// No mint and no fallback: an EMPTY string, so the caller sends the headline
// alone rather than a dead link — never a dead link, never silence.
func TestAwardClaimURLReturnsEmptyWithNoMintAndNoFallback(t *testing.T) {
	t.Setenv("MESHTK_RUN_INTERNAL_URL", "")
	t.Setenv("MESHTK_INTERNAL_SECRET", "")
	t.Setenv("MESHTK_RICKY_FALLBACK_URL", "")

	n := lyricsFleet()
	session := &lyricsSession{at: time.Now()}
	if got := n.awardClaimURL(context.Background(), 0, session, "ricky"); got != "" {
		t.Errorf("want empty url (headline-only degradation), got %q", got)
	}
}

// A nil session is treated as an uncached one, not a panic: the playback
// goroutine outlives nothing here, but a defensive nil must not crash the bot.
func TestAwardClaimURLToleratesNilSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"url":"https://q.defcon.run/a/nilsession12"}`))
	}))
	defer srv.Close()
	t.Setenv("MESHTK_RUN_INTERNAL_URL", srv.URL)
	t.Setenv("MESHTK_INTERNAL_SECRET", "s")

	n := lyricsFleet()
	if got := n.awardClaimURL(context.Background(), 0, nil, "ricky"); got != "https://q.defcon.run/a/nilsession12" {
		t.Errorf("nil session url = %q", got)
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
