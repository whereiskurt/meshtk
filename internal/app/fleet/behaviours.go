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
)

func (f *FleetCmd) behaviours(idx int, node *mqtt.Node, tic int) {
	tags := f.Config.Fleet[idx].BehaviourTag
	tagMap := make(map[string]bool)
	for _, tag := range tags {
		tagMap[tag] = true
	}

	if tagMap["nodeinfo"] {
		f.publishNodeInfo(idx, node)
	}

	if tagMap["movement"] {
		f.publishNextGPXMovement(idx, node, tagMap["gitter"])
	}
	if tagMap["position"] {
		f.publishPosition(idx, node, tagMap["gitter"])
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
	} else {
		f.Config.Log.Infof("Successfully decoded public key: %x\n", pk)
	}

	f.MqttClient[idx].PublishNodeInfo(node.From, ALL, whoamiTopic, node.LongName, node.ShortName, pk, meshtastic.HardwareModel(hwModelNumber), meshtastic.Config_DeviceConfig_CLIENT)
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
			f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Node[%d] moving...\n", idx, node.From)))

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
