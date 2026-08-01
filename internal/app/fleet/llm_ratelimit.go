package fleet

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultLLMCallsPerHour is the sustained per-radio ceiling on paid Bedrock
// Converse calls when the operator has not said otherwise.
//
// llm.go carries no limiter of any kind: every unlocked DM to a ghost becomes a
// metered call at llmMaxTokens 2000. The only throttle anywhere in that path is
// requestDedupWindow (30s), which collapses BYTE-IDENTICAL repeats so that
// Meshtastic's ~3x retransmits do not multiply a reply — varying a single
// character defeats it entirely. Sixty calls an hour sits far above any human
// conversation and roughly sixty times below what an unattended loop achieves.
//
// This bounds ONE radio. Aggregate spend across many distinct radios each
// sitting just under the bucket is deliberately NOT bounded here: a global
// fleet cap would let one attacker silence every ghost, and dead ghosts
// mid-event are a worse failure than a visible overage.
const defaultLLMCallsPerHour = 60

// llmRateWindow is the refill window. Capacity equals the hourly rate, so the
// knob reads as one plain sentence: at most N model calls per radio per hour,
// spendable as fast as the radio likes.
const llmRateWindow = time.Hour

// llmRateLimitReply answers a radio whose bucket is empty.
//
// It is in-persona and says nothing about a ceiling, a budget, a provider or
// any control at all — somebody who learns a ceiling exists has learned how to
// sit just underneath it. It invites a retry, so the path is never a dead end,
// and it is words rather than silence for the reason stageFullReply gives: a
// blackholed request is indistinguishable from a dead bot.
const llmRateLimitReply = "👻 …too many questions at once. give me a minute."

// llmBucket is one radio's balance against one ghost fleet. The balance is a
// float so the continuous refill needs no integer-truncation special case, and
// last is the moment that balance was brought up to date.
type llmBucket struct {
	tokens float64
	last   time.Time
}

// llmCallsPerHour reads the operator override for the per-radio ceiling.
//
// This reads LookupEnv, NOT the bare Getenv form lyricsMaxConcurrent uses,
// because the knob has kill-switch semantics and therefore has to tell "absent"
// apart from "explicitly set" — the same reason rickyChallenge reads LookupEnv.
//
// The zero-value decision is deliberate, and it is the point where this knob
// DEPARTS from lyricsMaxConcurrent, which maps 0 to its default. A zero lyric
// cap would silence ricky entirely with no operator upside. A zero CALL cap is
// a legitimate emergency brake: the ghosts keep answering — in words, with
// llmRateLimitReply — they simply stop costing money. So an explicit "0" IS the
// brake. Arrived at by typo it must not be, which is why only a cleanly parsed
// zero reaches it: blank, whitespace-only, non-numeric, negative and fractional
// values all fall back to the default instead.
func llmCallsPerHour() int {
	raw, ok := os.LookupEnv("MESHTK_LLM_CALLS_PER_HOUR")
	if !ok {
		return defaultLLMCallsPerHour
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v < 0 {
		return defaultLLMCallsPerHour
	}
	return v
}

// takeLLMToken is the pure half of the limiter, mirroring dedupRequest: bring
// the balance up to date, spend one token if one is there, and report whether
// the call may proceed. Time is a parameter rather than a dependency, which is
// what makes the refill testable without a clock seam and without a sleep.
//
// Nothing here can park the caller: it never sleeps, never starts a goroutine
// and never sends on a channel. That matters because handleLLMChat runs inline
// on paho's ordered dispatch goroutine, whose contract is that a handler must
// not block — a blocking acquire there stalls ACK dispatch and can outlast the
// fixed 30s requestDedupWindow, which is exactly the multi-copy bug observed
// live on 2026-07-19.
//
// A zero-value bucket starts FULL: a radio nobody has heard from gets its whole
// hourly allowance, which is what "fresh" ought to mean.
func takeLLMToken(b *llmBucket, capacity float64, window time.Duration, now time.Time) bool {
	if capacity <= 0 {
		// Zero is the operator kill switch (see llmCallsPerHour). A negative
		// capacity is a nonsense state and must not be read as "unlimited".
		return false
	}
	if b == nil || window <= 0 {
		return false
	}

	if b.last.IsZero() {
		b.tokens = capacity
		b.last = now
	}
	// A backwards clock refills nothing rather than handing out free tokens.
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * capacity / window.Seconds()
		if b.tokens > capacity {
			b.tokens = capacity
		}
		b.last = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// pruneLLMBuckets drops every radio idle for a full window. Such a bucket has
// provably refilled to capacity, so its state carries no information at all and
// keeping it is pure leak — without this the map gains one entry per distinct
// radio for the life of a multi-day fleet.
func pruneLLMBuckets(m map[uint32]*llmBucket, now time.Time, window time.Duration) {
	if window <= 0 {
		return
	}
	for k, b := range m {
		if b == nil || now.Sub(b.last) >= window {
			delete(m, k)
		}
	}
}

// allowLLMCall reports whether this radio may spend one Bedrock call against
// this ghost fleet, taking the token when it may. It is the FleetCmd half,
// mirroring isRetransmit: take the per-fleet mutex, prune, then take a token.
//
// It is non-blocking by construction — a map lookup and some arithmetic under a
// mutex, with nothing that can park the caller — for the reason spelled out on
// takeLLMToken above.
//
// Nil or short state degrades to UNLIMITED, never to a panic and never to
// refuse-everything, exactly as acquireLyricSlot does for its nil slot channel:
// every existing test in this package constructs a bare &FleetCmd{}, whose
// slices are nil, and indexing a nil slice panics. That is also why a bare
// &FleetCmd{} leaving LLMCallsPerHour at 0 is harmless — the guard below
// returns before the capacity is ever read.
//
// The bounds check is deliberately stricter than isRetransmit, which indexes
// RecentReq[toFleetIdx] bare: this method indexes two parallel slices, and a
// guard that clears one while the other is short still panics. NewFleets
// allocates them together so in practice the lengths match; the second check
// costs nothing and removes the assumption.
//
// Capacity comes from the LLMCallsPerHour field, resolved once in NewFleets,
// and never from the environment here — the same reasoning that sizes
// LyricSlots once, so a mid-flight env change cannot move the ceiling.
func (n *FleetCmd) allowLLMCall(toFleetIdx int, from uint32) bool {
	if n == nil {
		return true
	}
	if toFleetIdx < 0 || toFleetIdx >= len(n.LLMBuckets) || toFleetIdx >= len(n.LLMBucketMux) {
		return true
	}
	if n.LLMBuckets[toFleetIdx] == nil {
		return true
	}

	n.LLMBucketMux[toFleetIdx].Lock()
	defer n.LLMBucketMux[toFleetIdx].Unlock()

	m := n.LLMBuckets[toFleetIdx]
	now := time.Now()
	pruneLLMBuckets(m, now, llmRateWindow)

	b := m[from]
	if b == nil {
		b = &llmBucket{}
		m[from] = b
	}
	return takeLLMToken(b, float64(n.LLMCallsPerHour), llmRateWindow, now)
}
