package fleet

import "testing"

func TestMatchesTriggerCaseInsensitive(t *testing.T) {
	rt := &FlagChallengeRuntime{Triggers: []string{"hack the planet", "hacking the planet"}}
	if !matchesTrigger(rt, "so how do I HACK THE PLANET exactly") {
		t.Fatal("expected case-insensitive substring match")
	}
	if matchesTrigger(rt, "nice weather today") {
		t.Fatal("unexpected match")
	}
	if matchesTrigger(nil, "hack the planet") {
		t.Fatal("nil challenge must never match")
	}
}

func TestRenderReveal(t *testing.T) {
	rt := &FlagChallengeRuntime{RevealTemplate: "👻 flag: %CODE%", DerivedCode: "WVCSNLUF"}
	if got := renderReveal(rt); got != "👻 flag: WVCSNLUF" {
		t.Fatalf("renderReveal = %q", got)
	}
}
