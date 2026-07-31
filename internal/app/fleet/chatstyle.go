package fleet

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// dashRun matches an em or en dash together with any whitespace hugging it.
var dashRun = regexp.MustCompile(`\s*[\x{2014}\x{2013}]\s*`)

// normalizeDashes replaces em and en dashes with a comma and a space. The style
// preamble already forbids them, so this is the belt to that suspenders: a
// reply opening "Yo—Condor here" is the single most bot-looking thing we ship,
// and it costs nothing to make it unrepresentable.
func normalizeDashes(s string) string {
	s = strings.TrimSpace(s)
	s = dashRun.ReplaceAllString(s, ", ")

	// Remove leading ", " repeatedly (for multiple leading dashes).
	for strings.HasPrefix(s, ", ") {
		s = strings.TrimPrefix(s, ", ")
	}

	// Remove trailing ", " repeatedly (for multiple trailing dashes).
	for strings.HasSuffix(s, ", ") {
		s = strings.TrimSuffix(s, ", ")
	}

	return s
}

// splitSentenceAware breaks an over-long line into pieces of at most limit
// bytes, preferring the last sentence end inside the limit, then the last
// comma, then the last space, and hard-cutting only for an unbroken run.
func splitSentenceAware(text string, limit int) []string {
	// Guard against non-positive limits: neither hang nor panic is possible.
	if limit <= 0 {
		text = strings.TrimSpace(text)
		if text == "" {
			return []string{}
		}
		return []string{text}
	}

	var out []string
	rest := strings.TrimSpace(text)
	for len(rest) > limit {
		cut := lastIndexBefore(rest, limit, func(i int) bool {
			c := rest[i]
			return (c == '.' || c == '?' || c == '!') && i+1 < len(rest) && rest[i+1] == ' '
		})
		if cut > 0 {
			cut++ // keep the punctuation with the piece it ends
		}
		if cut <= 0 {
			cut = lastIndexBefore(rest, limit, func(i int) bool {
				return rest[i] == ',' && i+1 < len(rest) && rest[i+1] == ' '
			})
			if cut > 0 {
				cut++
			}
		}
		if cut <= 0 {
			cut = lastIndexBefore(rest, limit, func(i int) bool { return rest[i] == ' ' })
		}
		if cut <= 0 {
			// Hard cut path: make it rune-safe by backing off to rune boundary.
			cut = limit
			// Back off byte-by-byte until we reach a rune start.
			for cut > 0 && !utf8.RuneStart(rest[cut]) {
				cut--
			}
			// If we backed off to 0, a single rune is longer than limit.
			// Emit the whole first rune instead.
			if cut == 0 {
				_, size := utf8.DecodeRuneInString(rest)
				cut = size
			}
		}
		out = append(out, strings.TrimSpace(rest[:cut]))
		rest = strings.TrimSpace(rest[cut:])
	}
	if rest != "" {
		out = append(out, rest)
	}
	return out
}

// lastIndexBefore scans backwards from limit-1 for the highest index where
// match holds, returning -1 if there is none.
func lastIndexBefore(s string, limit int, match func(i int) bool) int {
	if limit > len(s) {
		limit = len(s)
	}
	for i := limit - 1; i > 0; i-- {
		if match(i) {
			return i
		}
	}
	return -1
}

// maxChatMessages caps how many transmissions one LLM reply may become. Each
// message is a separate PKI encrypt and a separate LoRa send, so this is an
// airtime budget, not a style preference.
const maxChatMessages = 7

// chatHardLimit is the per-message ceiling. The style preamble asks the model
// for 200 so its own lines never reach this; anything that does is a runaway
// line and gets handed to splitSentenceAware.
const chatHardLimit = 230

// splitMessages turns one LLM reply into the messages to send. The model
// authors its own breaks by emitting one message per line; this cleans up after
// it. dropped reports how many messages the cap discarded, so the caller can
// log it — a silent truncation would read as "the model was concise".
func splitMessages(reply string) (msgs []string, dropped int) {
	for _, line := range strings.Split(reply, "\n") {
		line = normalizeDashes(line)
		if line == "" {
			continue
		}
		if len(line) <= chatHardLimit {
			msgs = append(msgs, line)
			continue
		}
		msgs = append(msgs, splitSentenceAware(line, chatHardLimit)...)
	}
	if len(msgs) > maxChatMessages {
		dropped = len(msgs) - maxChatMessages
		msgs = msgs[:maxChatMessages]
	}
	return msgs, dropped
}

// baseDelay scales the pause after a message by that message's length, so a
// long message reads as having taken longer to thumb out.
func baseDelay(msgLen int) time.Duration {
	d := 450*time.Millisecond + time.Duration(msgLen)*14*time.Millisecond
	if d < 600*time.Millisecond {
		return 600 * time.Millisecond
	}
	if d > 3500*time.Millisecond {
		return 3500 * time.Millisecond
	}
	return d
}

// applyJitter spreads d by plus or minus 20%. r is expected in [0,1); the
// caller supplies it so the arithmetic stays deterministic under test.
func applyJitter(d time.Duration, r float64) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*r))
}

// openingDelay is the beat before the first message lands, so the ghost reads
// as having seen the question rather than having pre-computed the answer.
func openingDelay(r float64) time.Duration {
	return 700*time.Millisecond + time.Duration(r*800)*time.Millisecond
}
