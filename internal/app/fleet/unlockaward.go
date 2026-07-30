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

// notifyUnlockAward tells run.human a radio passed this ghost's TOTP unlock so
// the owning runner is granted the ghost's unlock flag. Best-effort: any
// failure is logged and swallowed — the unlock flow must never depend on it.
// Contract (mirrors claimlink.go):
//
//	POST {MESHTK_RUN_INTERNAL_URL}/api/internal/ctf/unlock-award
//	     x-internal-secret: {MESHTK_INTERNAL_SECRET}
//	     {"ghost":"ghost.goldstein","node":"!aabbccdd"}
func notifyUnlockAward(ctx context.Context, ghostId string, from uint32) error {
	base := os.Getenv("MESHTK_RUN_INTERNAL_URL")
	secret := os.Getenv("MESHTK_INTERNAL_SECRET")
	if base == "" || secret == "" {
		return errors.New("unlock-award not configured")
	}
	payload, _ := json.Marshal(map[string]string{
		"ghost": ghostId,
		"node":  fmt.Sprintf("!%08x", from),
	})
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", base+"/api/internal/ctf/unlock-award", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-internal-secret", secret)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unlock-award status %d", resp.StatusCode)
	}
	return nil
}
