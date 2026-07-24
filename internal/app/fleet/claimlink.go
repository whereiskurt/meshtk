package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Single-use flag-claim links: instead of revealing the derived flag code
// inline (shareable forever), the bot asks run.human to mint a short-ttl
// single-use claim nonce and DMs the claim URL. Contract:
//
//	POST {MESHTK_RUN_INTERNAL_URL}/api/internal/ctf/mint
//	     x-internal-secret: {MESHTK_INTERNAL_SECRET}
//	     {"ghost":"ghost.goldstein"}
//	→ 200 {"nonce":"…","url":"https://…/ctf/claim?nonce=…"}
//
// One link per radio per unlock session (cached on the OTPUnlock record, so it
// expires with the unlock). Any mint failure falls back to the pre-existing
// static-code reveal — a reveal never silently dies. The url/nonce is NEVER
// logged.

// mintClaimURL asks run.human for a fresh single-use claim link for ghostId.
// Unconfigured env or any transport/decode failure returns an error (the
// caller falls back to the static reveal).
func mintClaimURL(ctx context.Context, ghostId string) (string, error) {
	base := os.Getenv("MESHTK_RUN_INTERNAL_URL")
	secret := os.Getenv("MESHTK_INTERNAL_SECRET")
	if base == "" || secret == "" {
		return "", errors.New("claim-link mint not configured")
	}

	payload, _ := json.Marshal(map[string]string{"ghost": ghostId})
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", base+"/api/internal/ctf/mint", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-internal-secret", secret)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mint status %d", resp.StatusCode)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", errors.New("mint returned no url")
	}
	return out.URL, nil
}

// getOrMintRevealURL returns this radio's claim link for the current unlock
// session, minting AT MOST ONCE per unlock: the URL is cached on the radio's
// OTPUnlock record (so it dies with the unlock's 1-hour expiry) and a
// re-trigger re-sends the same link — no token farming.
func (n *FleetCmd) getOrMintRevealURL(ctx context.Context, toFleetIdx int, from uint32) (string, error) {
	n.OTPUnlockMux[toFleetIdx].Lock()
	rec := n.OTPUnlocks[toFleetIdx][from]
	cached := ""
	if rec != nil {
		cached = rec.RevealURL
	}
	n.OTPUnlockMux[toFleetIdx].Unlock()
	if cached != "" {
		return cached, nil
	}

	url, err := mintClaimURL(ctx, n.Config.Fleet[toFleetIdx].Id)
	if err != nil {
		return "", err
	}
	n.OTPUnlockMux[toFleetIdx].Lock()
	if rec := n.OTPUnlocks[toFleetIdx][from]; rec != nil {
		rec.RevealURL = url
	}
	n.OTPUnlockMux[toFleetIdx].Unlock()
	return url, nil
}

// sendFlagReveal delivers the covert-flag reveal for a matched trigger: two
// reliable DMs ("found a flag!" + the claim link), or — if minting fails for
// any reason — the pre-existing static-code reveal so the player is never left
// empty-handed.
func (n *FleetCmd) sendFlagReveal(toFleetIdx int, to uint32, from uint32, topic string, rt *FlagChallengeRuntime) {
	url, err := n.getOrMintRevealURL(context.Background(), toFleetIdx, from)
	if err != nil {
		if n.Config != nil && n.Config.Log != nil {
			n.Config.Log.Errorf("claim-link mint failed (fleet %d): %v — falling back to static reveal", toFleetIdx, err)
		}
		n.sendPKIReplyReliable(toFleetIdx, to, from, topic, renderReveal(rt))
		return
	}
	n.sendPKIReplyReliable(toFleetIdx, to, from, topic, "👻 You found a flag!")
	n.sendPKIReplyReliable(toFleetIdx, to, from, topic, url)
}
