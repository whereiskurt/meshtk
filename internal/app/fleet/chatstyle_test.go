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
