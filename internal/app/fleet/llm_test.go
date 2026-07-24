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
