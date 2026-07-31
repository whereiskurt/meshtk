package fleet

import (
	"regexp"
	"strings"
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
