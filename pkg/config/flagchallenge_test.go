package config

import "testing"

func TestParseFlagChallenges(t *testing.T) {
	raw := `{"ghost.goldstein":{"triggers":["hack the planet","hacking the planet"],"revealTemplate":"You found a flag! Code: {{code}}","committedCode":"hackers4evr"}}`
	m, err := ParseFlagChallenges(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, ok := m["ghost.goldstein"]
	if !ok {
		t.Fatal("ghost.goldstein missing")
	}
	if len(c.Triggers) != 2 || c.CommittedCode != "hackers4evr" {
		t.Fatalf("bad parse: %+v", c)
	}
	if !contains(c.RevealTemplate, "{{code}}") {
		t.Fatalf("reveal template missing {{code}}: %q", c.RevealTemplate)
	}
}

func TestParseFlagChallengesEmpty(t *testing.T) {
	m, err := ParseFlagChallenges("")
	if err != nil || len(m) != 0 {
		t.Fatalf("empty blob should yield empty map, got %v err=%v", m, err)
	}
}

func TestParseFlagChallengesRejectsTemplateWithoutPlaceholder(t *testing.T) {
	raw := `{"g":{"triggers":["x"],"revealTemplate":"no placeholder","committedCode":"c"}}`
	if _, err := ParseFlagChallenges(raw); err == nil {
		t.Fatal("expected error for reveal template missing {{code}}")
	}
}
