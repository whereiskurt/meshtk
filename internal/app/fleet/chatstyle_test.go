package fleet

import "testing"

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
