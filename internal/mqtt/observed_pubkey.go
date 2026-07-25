package mqtt

import (
	"fmt"
	"sync"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// observedPubKeys remembers the newest pubkey each node announced via
// NODEINFO, process-wide (mirrors pubKeyCache): every client subscription
// feeds one shared map, so a key observed on any topic is visible to all.
// Consumed by the OTP delivery poller as its last-resort recipient key when
// neither the user nor the authoritative MeshRadio row supplied one.
var observedPubKeys sync.Map // map[uint32]string ("0x" + 64 hex)

// noteObservedNodeInfo records the announced pubkey from a NODEINFO payload.
// Malformed payloads and keyless announcements are ignored silently — this
// sits on the hot dispatch path.
func noteObservedNodeInfo(from uint32, payload []byte) {
	user := new(meshtastic.User)
	if err := proto.Unmarshal(payload, user); err != nil {
		return
	}
	if pk := user.GetPublicKey(); len(pk) == 32 {
		observedPubKeys.Store(from, fmt.Sprintf("0x%x", pk))
	}
}

// ObservedPubKey returns the pubkey last announced via NODEINFO by nodeNum as
// ParseHexKey-ready 0x-hex, or ("", false) when the node is unknown/keyless.
func (c *MqttClient) ObservedPubKey(nodeNum uint32) (string, bool) {
	v, ok := observedPubKeys.Load(nodeNum)
	if !ok {
		return "", false
	}
	return v.(string), true
}
