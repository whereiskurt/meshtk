package mqtt

import (
	"bytes"
	"fmt"
	"testing"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// The OTP delivery poller's last-resort pubkey source is "whatever the radio
// itself announced via NODEINFO" — these lock the observation path end to end:
// a NODEINFO payload flowing through the dispatch must surface via
// ObservedPubKey as ParseHexKey-ready 0x-hex.
func TestObservedPubKeyFromNodeInfo(t *testing.T) {
	c := &MqttClient{}

	key := bytes.Repeat([]byte{0xab}, 32)
	payload, err := proto.Marshal(&meshtastic.User{
		Id:        "!7573fe10",
		LongName:  "Shannon_Overwatch",
		ShortName: "sha4",
		PublicKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}

	noteObservedNodeInfo(0x7573fe10, payload)

	got, ok := c.ObservedPubKey(0x7573fe10)
	if !ok {
		t.Fatal("expected observed pubkey")
	}
	if want := fmt.Sprintf("0x%x", key); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestObservedPubKeyMisses(t *testing.T) {
	c := &MqttClient{}

	if _, ok := c.ObservedPubKey(0xdeadbeef); ok {
		t.Fatal("unknown node must miss")
	}

	// A keyless NODEINFO (or garbage payload) must not store anything.
	payload, _ := proto.Marshal(&meshtastic.User{Id: "!00000001", LongName: "nokey"})
	noteObservedNodeInfo(0x00000001, payload)
	if _, ok := c.ObservedPubKey(0x00000001); ok {
		t.Fatal("keyless NODEINFO must not register")
	}
	noteObservedNodeInfo(0x00000002, []byte{0xff, 0xff, 0x01})
	if _, ok := c.ObservedPubKey(0x00000002); ok {
		t.Fatal("garbage payload must not register")
	}
}
