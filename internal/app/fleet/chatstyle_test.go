package fleet

import (
	"strings"
	"testing"
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
	for _, p := range splitSentenceAware(long, 230) {
		if len(p) > 230 {
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
