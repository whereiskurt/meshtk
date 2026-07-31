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

// The four reasons guardText generates ITSELF, meaning the sidecar could not be
// consulted at all. Every other reason it returns came FROM the sidecar and is
// a genuine policy decision. That distinction is the only thing separating "the
// bot refused you" from "the checker is down", and once the failmode is closed
// both arrive at the same !allowed branch — so it has to be readable from the
// reason string alone.
const (
	guardReasonBuildError  = "guard-build-error"
	guardReasonUnreachable = "guard-unreachable"
	guardReasonStatus      = "guard-status"
	guardReasonDecode      = "guard-decode"
)

// guardDegradedReply is what a player gets when the guardrail could not be
// consulted. Deliberately in the ghosts' voice, deliberately vague about the
// cause, and deliberately NOT the wording a genuine block gets: a reply that
// announced a security control was unavailable would tell an attacker probing
// the bot exactly when to push. Radio static is the register — it explains
// nothing and invites a retry.
const guardDegradedReply = "👻 …static on the line. try me again."

// isGuardOutageReason reports whether reason is one guardText minted because it
// could not reach a verdict, as opposed to a verdict the sidecar handed back.
func isGuardOutageReason(reason string) bool {
	switch reason {
	case guardReasonBuildError, guardReasonUnreachable, guardReasonStatus, guardReasonDecode:
		return true
	}
	return false
}

// guardRefusalMessage maps a guardText reason to the message to send. An outage
// degrades visibly; everything else — including an empty reason and any
// sidecar-supplied policy reason — keeps the existing canned refusal, so a real
// block still reads as a block.
//
// It lives here rather than in cmd.go so the reason strings and their
// interpretation stay in one file, and it is a pure function of the reason so
// both call sites can swap it in as an ARGUMENT rather than growing a branch.
func guardRefusalMessage(reason string) string {
	if isGuardOutageReason(reason) {
		return guardDegradedReply
	}
	return cannedRefusal
}

// logGuard emits one guardrail line, guarding the whole logger chain: guardText
// runs with a bare &FleetCmd{} in tests and during early bootstrap, where an
// unconditional Log call panics the process.
//
// The stable marker token is written at each CALL SITE rather than added here,
// because 72-04's CloudWatch metric filter is plain text over unstructured
// logrus output — an operator (and a grep) should find the token literally
// where the outage is emitted.
func (n *FleetCmd) logGuard(format string, args ...interface{}) {
	if n == nil || n.Config == nil || n.Config.Log == nil {
		return
	}
	n.Config.Log.Errorf(format, args...)
}

// guardText posts text to the localhost Guardrails-AI sidecar (§6.4). Contract:
//
//	POST {MESHTK_GUARDRAIL_URL}/guard  {"text":..,"direction":"input|output"}
//	→ 200 {"allowed":bool,"reason":string}
//
// If MESHTK_GUARDRAIL_URL is unset the stage is skipped (allow). On any
// transport error/timeout the MESHTK_GUARDRAIL_FAILMODE decides: "open"
// (default) allows so a sidecar hiccup never bricks the ghosts at the con;
// "closed" blocks.
//
// The second return is the REASON, and callers must treat it as load-bearing:
// pass it through guardRefusalMessage to pick the reply. Never log the guarded
// text itself on any branch.
func (n *FleetCmd) guardText(ctx context.Context, text string, src guardSource) (bool, string) {
	base := os.Getenv("MESHTK_GUARDRAIL_URL")
	if base == "" {
		return true, ""
	}
	failmode := os.Getenv("MESHTK_GUARDRAIL_FAILMODE")
	failClosed := failmode == "closed"
	allowOnErr := !failClosed

	payload, _ := json.Marshal(map[string]string{"text": text, "direction": string(src)})
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", base+"/guard", bytes.NewReader(payload))
	if err != nil {
		n.logGuard("MESHTK_GUARDRAIL_OUTAGE guardrail %s request build failed (%v); failmode=%s", src, err, failmode)
		return allowOnErr, guardReasonBuildError
	}
	req.Header.Set("content-type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		n.logGuard("MESHTK_GUARDRAIL_OUTAGE guardrail %s unreachable (%v); failmode=%s", src, err, failmode)
		return allowOnErr, guardReasonUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A 502 from the sidecar is as much an outage as a refused connection,
		// and this branch used to log nothing at all — so the alarm could not
		// see it. The status code is safe to log; the guarded text is not.
		n.logGuard("MESHTK_GUARDRAIL_OUTAGE guardrail %s returned status %d; failmode=%s", src, resp.StatusCode, failmode)
		return allowOnErr, guardReasonStatus
	}
	var out struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		n.logGuard("MESHTK_GUARDRAIL_OUTAGE guardrail %s response undecodable (%v); failmode=%s", src, err, failmode)
		return allowOnErr, guardReasonDecode
	}
	return out.Allowed, out.Reason
}
