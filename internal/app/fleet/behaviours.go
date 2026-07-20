package fleet

import (
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"strings"

	"github.com/whereiskurt/meshtk/internal/mqtt"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// movementDue reports whether this node should publish position/movement on
// this tic. every <= 1 means every tic (the default, unchanged behaviour).
// The nodeNum offset staggers nodes so a fleet does not emit all of its
// positions on the same tic.
func movementDue(every, tic int, nodeNum uint32) bool {
	if every <= 1 {
		return true
	}
	return (tic+int(nodeNum%uint32(every)))%every == 0
}

func (f *FleetCmd) behaviours(idx int, node *mqtt.Node, tic int) {
	tags := f.Config.Fleet[idx].BehaviourTag
	tagMap := make(map[string]bool)
	for _, tag := range tags {
		tagMap[tag] = true
	}

	if tagMap["nodeinfo"] {
		f.publishNodeInfo(idx, node)
	}

	// Position/movement is thinned independently of NodeInfo: it is half of all
	// channel traffic but the least useful half for a radio that just reconnected
	// and needs identities + pubkeys to DM anyone. Nodes are offset by their node
	// number so the surviving position publishes spread across tics instead of
	// bunching onto every Nth one.
	if movementDue(f.Config.Fleet[idx].MovementEveryTics, tic, node.From) {
		if tagMap["movement"] {
			f.publishNextGPXMovement(idx, node, tagMap["gitter"])
		}
		if tagMap["position"] {
			f.publishPosition(idx, node, tagMap["gitter"])
		}
	}

	if tic == 0 {
		if tagMap["sayhello"] {
			f.publishMessageToChannel(node, idx, fmt.Sprintf("%d:hello!", tic))
		}
	}
}

func (f *FleetCmd) publishMessageToChannel(node *mqtt.Node, idx int, message string) {
	const ALL = 0xffffffff
	whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)
	f.MqttClient[idx].PublishMessageEncrypted(node.From, ALL, whoamiTopic, meshtastic.PortNum_TEXT_MESSAGE_APP, []byte(message))
}

func (f *FleetCmd) publishNodeInfo(idx int, node *mqtt.Node) {
	const ALL = 0xffffffff

	whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)

	hwModelNumber, ok := meshtastic.HardwareModel_value[node.HwModel]
	if !ok {
		f.Config.Log.Warnf("⚠️ Unknown hardware model: %s. Defaulting to 0.\n", node.HwModel)
		hwModelNumber = 0
	}

	pk, err := hex.DecodeString(strings.TrimPrefix(node.PubKey, "0x"))
	if err != nil {
		f.Config.Log.Warnf("⚠️ Failed to decode public key: %v. Defaulting to empty key.\n", err)
		pk = []byte{}
	}

	f.MqttClient[idx].PublishNodeInfo(node.From, ALL, whoamiTopic, node.LongName, node.ShortName, pk, meshtastic.HardwareModel(hwModelNumber), meshtastic.Config_DeviceConfig_CLIENT)
}

// respondNodeInfo answers a directed NODEINFO_APP request (the app's "exchange
// user info" when a node is missing from the NodeDB) with this ghost's user
// info addressed to the requester. Real firmware replies to these; ignoring
// them left a freshly-wiped radio unable to learn a ghost's details on demand.
func (f *FleetCmd) respondNodeInfo(idx int, node *mqtt.Node, requester uint32) {
	whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)

	hwModelNumber, ok := meshtastic.HardwareModel_value[node.HwModel]
	if !ok {
		hwModelNumber = 0
	}

	pk, err := hex.DecodeString(strings.TrimPrefix(node.PubKey, "0x"))
	if err != nil {
		f.Config.Log.Warnf("⚠️ Failed to decode public key: %v. Defaulting to empty key.\n", err)
		pk = []byte{}
	}

	f.Config.Log.Infof("Answering user-info exchange: %08x -> requester %08x", node.From, requester)
	f.MqttClient[idx].PublishNodeInfoTo(node.From, requester, whoamiTopic, node.LongName, node.ShortName, pk, meshtastic.HardwareModel(hwModelNumber), meshtastic.Config_DeviceConfig_CLIENT)
}

func (f *FleetCmd) publishPosition(idx int, node *mqtt.Node, gitter bool) {
	fleet := f.Config.Fleet[idx]

	h := fnv.New64a()
	h.Write([]byte(fleet.Seed))
	seedValue := int64(h.Sum64())
	r := rand.New(rand.NewSource(int64(seedValue)))

	const ALL = 0xffffffff
	whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)

	var lat int32 = int32(node.Latitude)
	var lng int32 = int32(node.Longitude)
	var alt int32 = int32(node.Altitude)
	var prec uint32 = uint32(32)

	if gitter {
		scale := math.Cos(float64(node.Latitude) * math.Pi / 180.0)
		lat += r.Int31n(int32(fleet.LatLongAltGitter)*2) - int32(fleet.LatLongAltGitter) // +/- X
		lng += int32(float64(r.Int31n(int32(fleet.LatLongAltGitter)*2)-int32(fleet.LatLongAltGitter)) * scale)
	}
	f.MqttClient[idx].PublishPosition(node.From, ALL, whoamiTopic, lat, lng, alt, prec)
}

func (f *FleetCmd) publishNextGPXMovement(idx int, node *mqtt.Node, gitter bool) {

	fleet := f.Config.Fleet[idx]

	h := fnv.New64a()
	h.Write([]byte(fleet.Seed))
	seedValue := int64(h.Sum64())
	r := rand.New(rand.NewSource(int64(seedValue)))

	for _, m := range fleet.Movement {
		if len(m.GPXCoords) > 0 {
			// //f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Node[%d] moving...\n", idx, node.From)))

			node.ExtendedNode.GPSCoordinateOffset++

			//Bounding to index
			nextOffset := node.ExtendedNode.GPSCoordinateOffset
			if strings.Contains(m.Travel, "point-to-point") {
				nextOffset = ZigzagIndex(nextOffset, len(m.GPXCoords))
			} else if strings.Contains(m.Travel, "loop") {
				nextOffset = nextOffset % len(m.GPXCoords)
			}

			//Reverse offset direction
			if strings.Contains(m.Travel, "backward") {
				nextOffset = len(m.GPXCoords) - nextOffset - 1
			}
			node.Latitude = m.GPXCoords[nextOffset].Latitude
			node.Longitude = m.GPXCoords[nextOffset].Longitude

			if gitter {
				// Spread the lat/long/alt by +/- X to allow multiple folks 'at the same place' to be seen on the map
				scale := math.Cos(float64(node.Latitude) * math.Pi / 180.0)
				node.Latitude += r.Int31n(int32(fleet.LatLongAltGitter)*2) - int32(fleet.LatLongAltGitter) // +/- X
				node.Longitude += int32(float64(r.Int31n(int32(fleet.LatLongAltGitter)*2)-int32(fleet.LatLongAltGitter)) * scale)
			}

			node.Altitude = m.GPXCoords[nextOffset].Altitude
			node.Precision = uint32(m.GPXCoords[nextOffset].Precision)
			f.publishPosition(idx, node, gitter)
		}
	}
}

func (f *FleetCmd) publishACK(idx int, node *mqtt.Node, toNode uint32, requestId uint32) {
	whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)

	// A proper Meshtastic ACK is a Routing with error_reason = NONE. An empty
	// Routing{} marshals to zero bytes (no oneof variant set), which the
	// Meshtastic app surfaces as "Empty Ack Error". Set the variant explicitly
	// so the app decodes a clean delivery ack.
	routing := &meshtastic.Routing{
		Variant: &meshtastic.Routing_ErrorReason{
			ErrorReason: meshtastic.Routing_NONE,
		},
	}

	routingBytes, err := proto.Marshal(routing)
	if err != nil {
		f.Config.Log.Errorf("Failed to marshal ACK routing message: %v", err)
		return
	}

	f.MqttClient[idx].PublishACK(node.From, toNode, whoamiTopic, requestId, routingBytes)
}

func (f *FleetCmd) publishNodeInfoRequest(idx int, node *mqtt.Node, toNode uint32) {
	// Request nodeinfo from the target node
	whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)

	// Send an empty user message which triggers nodeinfo response
	user := &meshtastic.User{
		Id: fmt.Sprintf("!%08x", toNode), // Request info for this node
	}

	userBytes, err := proto.Marshal(user)
	if err != nil {
		f.Config.Log.Errorf("Failed to marshal nodeinfo request: %v", err)
		return
	}

	f.Config.Log.Debugf("Requesting nodeinfo from node %08x via node %08x", toNode, node.From)
	f.MqttClient[idx].PublishMessageEncrypted(node.From, toNode, whoamiTopic, meshtastic.PortNum_NODEINFO_APP, userBytes)
}

// ackEnabled reports whether the configured AckMode wires an ack handler at
// all. Only the explicit "off" disables acks; every other value (including
// empty, for configs that predate the knob) acks as before.
func ackEnabled(mode string) bool {
	return mode != "off"
}

func ZigzagIndex(i, total int) int {
	if total == 1 {
		return 0
	}
	dir := i / total
	col := i % total

	if dir%2 == 0 {
		// left to right
		return col
	}
	// left to right
	return total - 1 - col
}
