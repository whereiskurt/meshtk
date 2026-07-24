package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

type guardSource string

const (
	guardInput  guardSource = "input"
	guardOutput guardSource = "output"
)

// guardText posts text to the localhost Guardrails-AI sidecar (§6.4). Contract:
//   POST {MESHTK_GUARDRAIL_URL}/guard  {"text":..,"direction":"input|output"}
//   → 200 {"allowed":bool,"reason":string}
// If MESHTK_GUARDRAIL_URL is unset the stage is skipped (allow). On any transport
// error/timeout the MESHTK_GUARDRAIL_FAILMODE decides: "open" (default) allows so
// a sidecar hiccup never bricks the ghosts at the con; "closed" blocks.
func (n *FleetCmd) guardText(ctx context.Context, text string, src guardSource) (bool, string) {
	base := os.Getenv("MESHTK_GUARDRAIL_URL")
	if base == "" {
		return true, ""
	}
	failClosed := os.Getenv("MESHTK_GUARDRAIL_FAILMODE") == "closed"
	allowOnErr := !failClosed

	payload, _ := json.Marshal(map[string]string{"text": text, "direction": string(src)})
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", base+"/guard", bytes.NewReader(payload))
	if err != nil {
		return allowOnErr, "guard-build-error"
	}
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if n != nil && n.Config != nil && n.Config.Log != nil {
			n.Config.Log.Errorf("guardrail %s unreachable (%v); failmode=%s", src, err, os.Getenv("MESHTK_GUARDRAIL_FAILMODE"))
		}
		return allowOnErr, "guard-unreachable"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return allowOnErr, "guard-status"
	}
	var out struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return allowOnErr, "guard-decode"
	}
	return out.Allowed, out.Reason
}
