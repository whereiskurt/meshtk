package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FlagChallenge is the covert-flag mechanic for one ghost, delivered out-of-band
// via the MESHTK_FLAG_CHALLENGES env blob (SOPS→SSM→ECS) so it never appears in
// the committed persona prompt. CommittedCode is a DECOY HKDF input; the real
// revealed code is otp.DeriveFlagCode(serverSecret, fleetID, CommittedCode).
type FlagChallenge struct {
	Triggers       []string `json:"triggers"`
	RevealTemplate string   `json:"revealTemplate"` // must contain "%CODE%"
	CommittedCode  string   `json:"committedCode"`
}

// ParseFlagChallenges parses the MESHTK_FLAG_CHALLENGES JSON (a map keyed by
// fleet Id). An empty string yields an empty map (feature disabled). Every entry
// must carry a reveal template containing the %CODE% placeholder, else the
// derived code would have nowhere to land — fail loud at load, not at reveal.
func ParseFlagChallenges(raw string) (map[string]FlagChallenge, error) {
	out := map[string]FlagChallenge{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("MESHTK_FLAG_CHALLENGES: %w", err)
	}
	for id, c := range out {
		if !strings.Contains(c.RevealTemplate, "%CODE%") {
			return nil, fmt.Errorf("challenge %q: revealTemplate missing the %%CODE%% placeholder", id)
		}
	}
	return out, nil
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
