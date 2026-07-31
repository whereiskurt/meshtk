package fleet

import (
	"strings"
	"testing"
)

func TestParseAnthropicResponse(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"hack "},{"type":"text","text":"the planet"}]}`)
	got, err := parseAnthropicResponse(body)
	if err != nil || got != "hack the planet" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestParseAnthropicResponseError(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"nope"}}`)
	if _, err := parseAnthropicResponse(body); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("expected surfaced error, got %v", err)
	}
}

func TestAnthropicModelBodyShape(t *testing.T) {
	b := anthropicModelBody("claude-haiku-4-5", "sys", "msg", 150, 0.8)
	s := string(b)
	for _, want := range []string{`"model":"claude-haiku-4-5"`, `"system":"sys"`, `"max_tokens":150`, `"role":"user"`, `"content":"msg"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("body missing %s: %s", want, s)
		}
	}
}

func TestComposeSystemPromptIncludesBoth(t *testing.T) {
	got := composeSystemPrompt("You are Condor.")
	if !strings.Contains(got, "You are Condor.") {
		t.Error("persona missing from composed prompt")
	}
	if !strings.Contains(got, "own line") {
		t.Error("style preamble missing from composed prompt")
	}
}

func TestComposeSystemPromptEmptyPersona(t *testing.T) {
	if got := composeSystemPrompt("   "); got != chatStylePreamble {
		t.Errorf("empty persona should yield the bare preamble, got %q", got)
	}
}

// The preamble tells the model never to use an em dash. It would be a poor
// teacher if it used one itself.
func TestChatStylePreambleHasNoDashes(t *testing.T) {
	if strings.ContainsAny(chatStylePreamble, "—–") {
		t.Error("chatStylePreamble contains an em or en dash")
	}
}

func TestLLMMaxTokensAllowsFullBurst(t *testing.T) {
	// 7 messages of ~200 chars is roughly 400 tokens; the ceiling must clear
	// that with room so the final message is never clipped mid-word.
	if llmMaxTokens < 1000 {
		t.Errorf("llmMaxTokens = %d, too low for a 7-message burst", llmMaxTokens)
	}
}
