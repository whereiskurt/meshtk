package fleet

import (
	"sync"
	"testing"

	"github.com/whereiskurt/meshtk/internal/mqtt"
	"github.com/whereiskurt/meshtk/pkg/config"
)

func TestOverrideFleetKeysGated(t *testing.T) {
	node := mqtt.NewNode("msh/US/2/e/dc.run")
	node.From = 2076591764
	node.PubKey = "0xcommittedpub"
	node.PrivKey = "0xcommittedpriv"

	f := &FleetCmd{}
	f.Config = &config.Config{}
	f.Nodes = []mqtt.NodeDB{{node.From: node}}
	f.NodesMutex = make([]sync.Mutex, 1)

	// Secret absent: committed keys retained.
	f.overrideFleetKeys(0)
	if node.PrivKey != "0xcommittedpriv" {
		t.Fatalf("expected committed key retained, got %s", node.PrivKey)
	}

	// Secret present: keys overridden with derived values.
	f.Config.GhostKeySecret = "top-secret"
	f.overrideFleetKeys(0)
	_, wantPriv, _ := mqtt.DeriveNodeKey("top-secret", 2076591764)
	if node.PrivKey == "0xcommittedpriv" {
		t.Fatal("expected key to be overridden when secret is set")
	}
	if node.PrivKey != mqtt.HexKey(wantPriv) {
		t.Fatalf("privkey = %s, want %s", node.PrivKey, mqtt.HexKey(wantPriv))
	}
}
