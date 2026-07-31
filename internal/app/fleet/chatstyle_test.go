package fleet

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNormalizeDashes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Yo—Condor here", "Yo, Condor here"},
		{"Yo — Condor here", "Yo, Condor here"},
		{"a–b", "a, b"},
		{"—leading", "leading"},
		{"trailing—", "trailing"},
		{"no dashes here", "no dashes here"},
		{"  padded  ", "padded"},
		{"Copy that, standing by,", "Copy that, standing by,"},
		{"——leading", "leading"},
	}
	for _, c := range cases {
		if got := normalizeDashes(c.in); got != c.want {
			t.Errorf("normalizeDashes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitSentenceAwarePrefersSentenceEnd(t *testing.T) {
	// Break should land after "one." not mid-word.
	in := "aaa bbb one. ccc ddd two"
	got := splitSentenceAware(in, 14)
	want := []string{"aaa bbb one.", "ccc ddd two"}
	if len(got) != len(want) {
		t.Fatalf("got %d parts %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("part %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitSentenceAwareFallsBackToComma(t *testing.T) {
	got := splitSentenceAware("aaa bbb ccc, ddd eee fff", 14)
	if got[0] != "aaa bbb ccc," {
		t.Errorf("part 0 = %q, want %q", got[0], "aaa bbb ccc,")
	}
}

func TestSplitSentenceAwareFallsBackToSpace(t *testing.T) {
	got := splitSentenceAware("aaa bbb ccc ddd eee fff", 14)
	if got[0] != "aaa bbb ccc" {
		t.Errorf("part 0 = %q, want %q", got[0], "aaa bbb ccc")
	}
}

func TestSplitSentenceAwareHardCutsUnbrokenRun(t *testing.T) {
	got := splitSentenceAware("aaaaaaaaaaaaaaaaaaaaaaaa", 10)
	if len(got) != 3 || got[0] != "aaaaaaaaaa" {
		t.Fatalf("got %q, want 3 parts starting with 10 a's", got)
	}
}

func TestSplitSentenceAwareRespectsLimit(t *testing.T) {
	long := "The tech was never the hard part. People are. A badge and a clipboard got me through more doors than any exploit ever did, and that is not a joke about security theatre, it is just what happened every single time I tried it."
	for _, p := range splitSentenceAware(long, 200) {
		if len(p) > 200 {
			t.Errorf("part exceeds limit at %d: %q", len(p), p)
		}
	}
}

func TestSplitSentenceAwareGuardsNonPositiveLimit(t *testing.T) {
	// Must not hang and must not panic.
	if got := splitSentenceAware("some text", 0); len(got) == 0 {
		t.Error("limit 0 should still return the text")
	}
	if got := splitSentenceAware("some text", -5); len(got) == 0 {
		t.Error("negative limit should still return the text")
	}
}

func TestSplitSentenceAwareHardCutKeepsRunesIntact(t *testing.T) {
	// No spaces anywhere, so every break takes the hard-cut path, and the
	// multi-byte runes make byte-slicing visible.
	in := strings.Repeat("é", 40) // 80 bytes, zero spaces
	for _, p := range splitSentenceAware(in, 25) {
		if !utf8.ValidString(p) {
			t.Errorf("hard cut produced invalid UTF-8: %q", p)
		}
	}
}

func TestSplitSentenceAwarePreservesContent(t *testing.T) {
	// Guards against off-by-one slicing: the pieces must reconstruct the
	// input, ignoring the whitespace the splitter trims at each break.
	in := "The tech was never the hard part. People are. A badge and a clipboard got me through more doors than any exploit ever did, and that is not a joke about security theatre, it is just what happened every single time I tried it."
	joined := strings.Join(splitSentenceAware(in, 60), " ")
	if strings.Join(strings.Fields(joined), " ") != strings.Join(strings.Fields(in), " ") {
		t.Errorf("content not preserved:\n got %q\nwant %q", joined, in)
	}
}

func TestSplitMessagesOnePerLine(t *testing.T) {
	reply := "Condor here.\n\nWhat's good is nobody asks that anymore.\nI never broke crypto. I broke assumtions.\n*assumptions\n"
	msgs, dropped := splitMessages(reply)
	want := []string{
		"Condor here.",
		"What's good is nobody asks that anymore.",
		"I never broke crypto. I broke assumtions.",
		"*assumptions",
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d msgs %q, want %d", len(msgs), msgs, len(want))
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("msg %d = %q, want %q", i, msgs[i], want[i])
		}
	}
}

func TestSplitMessagesNormalizesDashes(t *testing.T) {
	msgs, _ := splitMessages("Yo—Condor here.")
	if msgs[0] != "Yo, Condor here." {
		t.Errorf("got %q, want %q", msgs[0], "Yo, Condor here.")
	}
}

func TestSplitMessagesCapsAtSeven(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, "line")
	}
	msgs, dropped := splitMessages(strings.Join(lines, "\n"))
	if len(msgs) != maxChatMessages {
		t.Errorf("got %d msgs, want %d", len(msgs), maxChatMessages)
	}
	if dropped != 3 {
		t.Errorf("dropped = %d, want 3", dropped)
	}
}

func TestSplitMessagesExpandsOverLongLine(t *testing.T) {
	long := strings.Repeat("word ", 100) // 500 bytes, no sentence ends
	msgs, _ := splitMessages(long)
	if len(msgs) < 2 {
		t.Fatalf("expected the long line to expand, got %d", len(msgs))
	}
	for _, m := range msgs {
		if len(m) > chatHardLimit {
			t.Errorf("msg exceeds hard limit at %d: %q", len(m), m)
		}
	}
}

func TestSplitMessagesEmptyReply(t *testing.T) {
	msgs, dropped := splitMessages("\n  \n\n")
	if len(msgs) != 0 || dropped != 0 {
		t.Errorf("got %d msgs / %d dropped, want 0/0", len(msgs), dropped)
	}
}

func TestBaseDelayClamps(t *testing.T) {
	cases := []struct {
		msgLen int
		want   time.Duration
	}{
		{0, 600 * time.Millisecond},    // floor
		{10, 600 * time.Millisecond},   // 450+140=590, still floored
		{130, 2270 * time.Millisecond}, // 450 + 130*14
		{200, 3250 * time.Millisecond}, // 450+2800=3250, headroom under the 3500ms ceiling at the new chatHardLimit
		{5000, 3500 * time.Millisecond},
	}
	for _, c := range cases {
		if got := baseDelay(c.msgLen); got != c.want {
			t.Errorf("baseDelay(%d) = %v, want %v", c.msgLen, got, c.want)
		}
	}
}

// Bounds rather than equality on purpose: 0.8+0.4*1 lands on a float64 knife
// edge where the product can round either side of 1.2e9 ns, and an exact
// assertion would be flaky rather than wrong.
func TestApplyJitterSpansTwentyPercent(t *testing.T) {
	d := 1000 * time.Millisecond
	within := func(name string, got, want time.Duration) {
		if got < want-time.Millisecond || got > want+time.Millisecond {
			t.Errorf("%s = %v, want ~%v", name, got, want)
		}
	}
	within("applyJitter(1s, 0)", applyJitter(d, 0), 800*time.Millisecond)
	within("applyJitter(1s, 1)", applyJitter(d, 1), 1200*time.Millisecond)
	within("applyJitter(1s, 0.5)", applyJitter(d, 0.5), 1000*time.Millisecond)
}

func TestOpeningDelayRange(t *testing.T) {
	if got := openingDelay(0); got != 700*time.Millisecond {
		t.Errorf("openingDelay(0) = %v, want 700ms", got)
	}
	if got := openingDelay(1); got != 1500*time.Millisecond {
		t.Errorf("openingDelay(1) = %v, want 1500ms", got)
	}
}
