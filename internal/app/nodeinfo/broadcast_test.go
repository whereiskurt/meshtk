package nodeinfo

import (
	"testing"

	"github.com/whereiskurt/meshtk/pkg/config"
)

// Announce's ticker calls DoBroadcast every BroadcastIntervalSec for the life
// of the process, so a text message published here is not a one-off greeting --
// it is a channel-wide chat message repeated forever to every radio holding the
// channel key. Historically the payload was a hardcoded "hello world", which
// meant any node running `nodeinfo announce` (the meshmap collector included)
// spammed the mesh whether or not anyone asked it to.

func TestBroadcastTextSilentWhenUnconfigured(t *testing.T) {
	c := config.NewConfig()
	c.NodeInfo.BroadcastMessage = ""

	payload, ok := broadcastText(c)

	if ok {
		t.Errorf("broadcastText returned %q to publish with no BroadcastMessage set; want no text publish at all", payload)
	}
}

func TestBroadcastTextUsesConfiguredMessage(t *testing.T) {
	c := config.NewConfig()
	c.NodeInfo.BroadcastMessage = "trail run at 0700"

	payload, ok := broadcastText(c)

	if !ok {
		t.Fatal("broadcastText suppressed an explicitly configured BroadcastMessage")
	}
	if string(payload) != "trail run at 0700" {
		t.Errorf("payload = %q, want %q", payload, "trail run at 0700")
	}
}
