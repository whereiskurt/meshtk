package otp

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net/url"
)

// DeriveTotpSecret deterministically derives the REAL base32 TOTP secret for a
// fleet entry from a server-only secret, the entry's stable Id, and the secret
// committed in the config's OtpUrl. The committed value is only an HKDF input —
// a decoy that never validates anything once derivation is active — so rotating
// it in the YAML rotates the effective seed without touching the server secret.
// Same (serverSecret, fleetID, committedSecret) yields the same secret on every
// call. This is the OTP twin of internal/mqtt's DeriveNodeKey: HKDF-SHA256,
// nil salt, a domain-separated info label, here with 20 output bytes encoded as
// unpadded uppercase RFC 4648 base32 (a standard authenticator-app secret).
func DeriveTotpSecret(serverSecret, fleetID, committedSecret string) (string, error) {
	info := fmt.Sprintf("meshtk-otp-seed:%s:%s", fleetID, committedSecret)
	key, err := hkdf.Key(sha256.New, []byte(serverSecret), nil, info, 20)
	if err != nil {
		return "", fmt.Errorf("hkdf: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key), nil
}

// DeriveOtpUrl returns otpURL with ONLY its secret query param replaced by the
// derived secret. Every other param (algorithm, digits, period, issuer) and the
// label pass through, so authenticator enrollments keep their display identity.
func DeriveOtpUrl(serverSecret, fleetID, otpURL string) (string, error) {
	u, err := url.Parse(otpURL)
	if err != nil {
		return "", fmt.Errorf("parse otp url: %w", err)
	}
	q := u.Query()
	committed := q.Get("secret")
	if committed == "" {
		return "", fmt.Errorf("otp url has no secret param")
	}
	derived, err := DeriveTotpSecret(serverSecret, fleetID, committed)
	if err != nil {
		return "", err
	}
	q.Set("secret", derived)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
