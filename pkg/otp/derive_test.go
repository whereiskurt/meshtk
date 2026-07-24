package otp

import (
	"net/url"
	"strings"
	"testing"
)

// Shared cross-implementation vectors: the SAME table lives in run.human's
// mesh-otp-derive vitest (defcon.run.34 apps/run.human/webapp/src/lib/
// __tests__/mesh-otp-derive.test.ts). If either side changes, both fail.
var deriveVectors = []struct {
	serverSecret string
	fleetID      string
	committed    string
	derived      string
}{
	{"test-server-secret", "ghost.goldstein", "GZRGQNKGKN4DINQ", "XHNN23O25QAZITZ4CZTCXU4NIR6LRRCK"},
	{"test-server-secret", "ghost.mudge", "NA2DG", "7KS3JJBI5CD6POHUUCC6XWHE65TXHGGX"},
	{"another-secret", "ghost.condor", "EZRWO", "74M2OE6WWHRXQYZUBYC6ZJ6TX5NKGRDR"},
}

func TestDeriveTotpSecretVectors(t *testing.T) {
	for _, v := range deriveVectors {
		got, err := DeriveTotpSecret(v.serverSecret, v.fleetID, v.committed)
		if err != nil {
			t.Fatalf("DeriveTotpSecret(%s): %v", v.fleetID, err)
		}
		if got != v.derived {
			t.Errorf("DeriveTotpSecret(%s) = %s, want %s", v.fleetID, got, v.derived)
		}
	}
}

func TestDeriveTotpSecretDomainSeparation(t *testing.T) {
	a, _ := DeriveTotpSecret("s", "ghost.a", "SEED")
	b, _ := DeriveTotpSecret("s", "ghost.b", "SEED")
	c, _ := DeriveTotpSecret("s2", "ghost.a", "SEED")
	d, _ := DeriveTotpSecret("s", "ghost.a", "SEED2")
	if a == b || a == c || a == d {
		t.Errorf("expected distinct secrets across id/server/committed changes: %s %s %s %s", a, b, c, d)
	}
}

func TestDeriveOtpUrlSwapsOnlySecret(t *testing.T) {
	in := "otpauth://totp/Emmanuel%20Goldstein?secret=GZRGQNKGKN4DINQ&issuer=Defcon.run&algorithm=SHA1&digits=6&period=120"
	out, err := DeriveOtpUrl("test-server-secret", "ghost.goldstein", in)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if got := q.Get("secret"); got != "XHNN23O25QAZITZ4CZTCXU4NIR6LRRCK" {
		t.Errorf("secret = %s", got)
	}
	for k, want := range map[string]string{"issuer": "Defcon.run", "algorithm": "SHA1", "digits": "6", "period": "120"} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	if !strings.Contains(out, "Emmanuel") {
		t.Errorf("label lost: %s", out)
	}
	// The rewritten URL must still parse through the normal handler path.
	if _, err := NewOTPHandler(out); err != nil {
		t.Errorf("NewOTPHandler(rewritten): %v", err)
	}
}

func TestDeriveOtpUrlRejectsSecretless(t *testing.T) {
	if _, err := DeriveOtpUrl("s", "ghost.x", "otpauth://totp/Nope?issuer=Defcon.run"); err == nil {
		t.Error("expected error for missing secret param")
	}
}

// Shared cross-implementation vectors for the covert flag code — the SAME table
// lives in run.human's mesh-otp-derive vitest (defcon.run.34). 5 bytes → 8-char
// unpadded uppercase base32.
var flagVectors = []struct {
	serverSecret string
	fleetID      string
	committed    string
	derived      string
}{
	{"test-server-secret", "ghost.goldstein", "hackers4evr", "WVCSNLUF"},
	{"test-server-secret", "ghost.mudge", "0g3l33t", "FNUUESUC"},
	{"another-secret", "ghost.condor", "fr33k3v1n", "4JCPQVLU"},
}

func TestDeriveFlagCodeVectors(t *testing.T) {
	for _, v := range flagVectors {
		got, err := DeriveFlagCode(v.serverSecret, v.fleetID, v.committed)
		if err != nil {
			t.Fatalf("DeriveFlagCode(%s): %v", v.fleetID, err)
		}
		if got != v.derived {
			t.Errorf("DeriveFlagCode(%s) = %s, want %s", v.fleetID, got, v.derived)
		}
		if len(got) != 8 {
			t.Errorf("DeriveFlagCode(%s) len = %d, want 8", v.fleetID, len(got))
		}
	}
}

func TestDeriveFlagCodeDomainSeparation(t *testing.T) {
	a, _ := DeriveFlagCode("s", "ghost.a", "CODE")
	b, _ := DeriveFlagCode("s", "ghost.b", "CODE")
	c, _ := DeriveFlagCode("s2", "ghost.a", "CODE")
	d, _ := DeriveFlagCode("s", "ghost.a", "CODE2")
	if a == b || a == c || a == d {
		t.Errorf("expected distinct flag codes across id/server/committed changes: %s %s %s %s", a, b, c, d)
	}
	// Flag code and TOTP secret must NOT collide for the same inputs (distinct info labels).
	tot, _ := DeriveTotpSecret("s", "ghost.a", "CODE")
	if a == tot {
		t.Errorf("flag code and totp secret collided: %s", a)
	}
}
