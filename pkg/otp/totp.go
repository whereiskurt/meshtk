package otp

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TOTPConfig stores the configuration for TOTP generation
type TOTPConfig struct {
	Secret      string
	Digits      int
	Period      int
	Algorithm   string
	Issuer      string
	AccountName string
	URL         string
	Cached      map[string]map[string]string // Cache for TOTP values
}

// NewOTPHandler parses an otpauth URL and returns the TOTP configuration
func NewOTPHandler(otpURL string) (*TOTPConfig, error) {
	u, err := url.Parse(otpURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OTP URL: %w", err)
	}
	if u.Scheme != "otpauth" {
		return nil, fmt.Errorf("invalid OTP URL scheme: %s", u.Scheme)
	}

	if u.Host != "totp" {
		return nil, fmt.Errorf("unsupported OTP type: %s", u.Host)
	}

	// Remove leading slash from path
	accountName := strings.TrimPrefix(u.Path, "/")

	// Parse query parameters
	query := u.Query()
	secret := query.Get("secret")
	if secret == "" {
		return nil, fmt.Errorf("secret is required")
	}

	// Default values
	digits := 6
	period := 120
	algorithm := "SHA1"
	issuer := "Defcon.run"

	// Override with provided values if present
	if d := query.Get("digits"); d != "" {
		digits, err = strconv.Atoi(d)
		if err != nil {
			return nil, fmt.Errorf("invalid digits value: %s", d)
		}
	}

	if p := query.Get("period"); p != "" {
		period, err = strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid period value: %s", p)
		}
	}

	if a := query.Get("algorithm"); a != "" {
		algorithm = strings.ToUpper(a)
	}

	if i := query.Get("issuer"); i != "" {
		issuer = i
	}

	return &TOTPConfig{
		Secret:      secret,
		Digits:      digits,
		Period:      period,
		Algorithm:   algorithm,
		Issuer:      issuer,
		AccountName: accountName,
		URL:         otpURL,
		Cached:      make(map[string]map[string]string),
	}, nil
}

// GenerateTOTP generates a TOTP for a specific time
func (tc *TOTPConfig) GenerateTOTP(timestamp time.Time) (string, error) {
	// Normalize the secret (remove spaces and handle base32 padding)
	secret := strings.ToUpper(strings.ReplaceAll(tc.Secret, " ", ""))
	// Add padding if necessary
	if len(secret)%8 != 0 {
		secret = secret + strings.Repeat("=", 8-len(secret)%8)
	}

	// Decode the secret
	key, err := base32.StdEncoding.DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("failed to decode secret: %w", err)
	}

	// Calculate the counter value (number of time steps since epoch)
	counter := timestamp.Unix() / int64(tc.Period)

	// Generate the HMAC-SHA1 hash
	counterBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(counterBytes, uint64(counter))

	var mac []byte
	switch tc.Algorithm {
	case "SHA1":
		h := hmac.New(sha1.New, key)
		h.Write(counterBytes)
		mac = h.Sum(nil)
	default:
		return "", fmt.Errorf("unsupported algorithm: %s", tc.Algorithm)
	}

	// Dynamic truncation
	offset := mac[len(mac)-1] & 0x0f
	binary := ((int(mac[offset]) & 0x7f) << 24) |
		((int(mac[offset+1]) & 0xff) << 16) |
		((int(mac[offset+2]) & 0xff) << 8) |
		(int(mac[offset+3]) & 0xff)

	// Generate the OTP value
	otp := binary % int(pow10(tc.Digits))
	otpStr := fmt.Sprintf("%0*d", tc.Digits, otp)

	return otpStr, nil
}

// ValidCodesWindow returns the TOTP codes for the current period plus `each`
// periods on either side. A single ±1 window (CalculateTOTPWithAdjacentPeriods)
// is only ~90s at period=30 — too tight for a LoRa mesh round-trip, where a code
// is read off a phone, typed into a radio, transmitted, and relayed over several
// hops before it reaches the bot, so a short-period code routinely expires in
// flight. A wider window keeps period=30 (which phone authenticator apps honor,
// unlike 120) while tolerating that latency. At period=30, each=5 accepts ~5.5
// minutes of transit. The wider replay window is acceptable for a CTF unlock.
func (tc *TOTPConfig) ValidCodesWindow(each int) ([]string, error) {
	if each < 0 {
		each = 0
	}
	nowUnix := time.Now().Unix()
	start := nowUnix / int64(tc.Period) * int64(tc.Period)
	codes := make([]string, 0, 2*each+1)
	for i := -each; i <= each; i++ {
		t := time.Unix(start+int64(i)*int64(tc.Period), 0)
		code, err := tc.GenerateTOTP(t)
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// CalculateTOTPWithAdjacentPeriods calculates the TOTP for the current period,
// as well as the previous and next periods
func (tc *TOTPConfig) CalculateTOTPWithAdjacentPeriods() (map[string]string, error) {

	now := time.Now()

	// Calculate the start of the current period
	currentPeriodStart := now.Unix() / int64(tc.Period) * int64(tc.Period)

	// Generate timestamps for current, previous, and next periods
	currentTime := time.Unix(currentPeriodStart, 0)
	previousTime := time.Unix(currentPeriodStart-int64(tc.Period), 0)
	nextTime := time.Unix(currentPeriodStart+int64(tc.Period), 0)

	key := currentTime.Format(time.RFC3339)

	// Calculate remaining seconds in current period
	remainingSeconds := tc.Period - int(now.Unix()%int64(tc.Period))

	// Generate TOTPs
	currentTOTP, err := tc.GenerateTOTP(currentTime)
	if err != nil {
		return nil, err
	}

	previousTOTP, err := tc.GenerateTOTP(previousTime)
	if err != nil {
		return nil, err
	}

	nextTOTP, err := tc.GenerateTOTP(nextTime)
	if err != nil {
		return nil, err
	}

	ret := map[string]string{
		"current":            currentTOTP,
		"previous":           previousTOTP,
		"next":               nextTOTP,
		"remainingSeconds":   fmt.Sprintf("%d", remainingSeconds),
		"period":             fmt.Sprintf("%d", tc.Period),
		"currentPeriodStart": currentTime.Format(time.RFC3339),
		"nextPeriodStart":    nextTime.Format(time.RFC3339),
	}

	tc.Cached[key] = ret

	return ret, nil
}

// Helper function to calculate 10^n
func pow10(n int) int64 {
	result := int64(1)
	for i := 0; i < n; i++ {
		result *= 10
	}
	return result
}
