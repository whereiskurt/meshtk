package otp

import "testing"

func TestValidCodesWindow(t *testing.T) {
	h, err := NewOTPHandler("otpauth://totp/T?secret=GZRGQNKGKN4DINQ&period=30&digits=6&algorithm=SHA1")
	if err != nil {
		t.Fatal(err)
	}
	// ±5 → 11 codes; the middle one equals the current adjacent "current".
	codes, err := h.ValidCodesWindow(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 11 {
		t.Fatalf("len=%d, want 11", len(codes))
	}
	adj, _ := h.CalculateTOTPWithAdjacentPeriods()
	// The window must contain prev/current/next (superset of the old behavior).
	for _, k := range []string{"previous", "current", "next"} {
		found := false
		for _, c := range codes {
			if c == adj[k] {
				found = true
			}
		}
		if !found {
			t.Errorf("window missing %s code %s", k, adj[k])
		}
	}
	// each=0 → just the current period.
	one, _ := h.ValidCodesWindow(0)
	if len(one) != 1 || one[0] != adj["current"] {
		t.Errorf("each=0 got %v, want [%s]", one, adj["current"])
	}
}
