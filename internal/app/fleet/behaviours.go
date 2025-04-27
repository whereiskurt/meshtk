package fleet

import (
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
		f.nodeinfo(idx, node)
	}

	// Make moves, and then tell folks.
	if tagMap["movement"] {
		f.gpxMovement(idx, node, tic)
	}

	if tagMap["position"] {
		f.position(idx, node)
	}

	if tagMap["gitter"] {
		// f.gitter(idx, node, tic)
	}

	if tic == 0 {
		if tagMap["sayhello"] {
			const ALL = 0xffffffff
			whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)
			f.MqttClient[idx].PublishMessageEncrypted(node.From, ALL, whoamiTopic, meshtastic.PortNum_TEXT_MESSAGE_APP, []byte("Hello world!"))
		}
	}
}

func (f *FleetCmd) nodeinfo(idx int, node *mqtt.Node) {
	const ALL = 0xffffffff

	whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)

	hwModelNumber, ok := meshtastic.HardwareModel_value[node.HwModel]
	if !ok {
		f.Config.Log.Warnf("⚠️ Unknown hardware model: %s. Defaulting to 0.\n", node.HwModel)
		hwModelNumber = 0
	}
	f.MqttClient[idx].PublishNodeInfo(node.From, ALL, whoamiTopic, node.LongName, node.ShortName, meshtastic.HardwareModel(hwModelNumber), meshtastic.Config_DeviceConfig_CLIENT)

}

func (f *FleetCmd) position(idx int, node *mqtt.Node) {
	const ALL = 0xffffffff
	whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)

	var lat int32 = int32(node.Latitude)
	var lng int32 = int32(node.Longitude)
	var alt int32 = int32(node.Altitude)
	var prec uint32 = uint32(32)

	f.MqttClient[idx].PublishPosition(node.From, ALL, whoamiTopic, lat, lng, alt, prec)
}

func (f *FleetCmd) gpxMovement(idx int, node *mqtt.Node, tic int) {

	fleet := f.Config.Fleet[idx]
	h := fnv.New64a()
	h.Write([]byte(fleet.Seed))
	seedValue := int64(h.Sum64())

	r := rand.New(rand.NewSource(int64(seedValue)))
	for _, m := range fleet.Movement {
		if len(m.GPXCoords) > 0 {
			f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Node[%d] moving...\n", idx, node.From)))

			direction := 1 // Default direction is forward
			if strings.Contains(m.Travel, "backward") {
				direction = -1
			}

			//TOOO: Come back and re-think this. Do we need tic?
			nextOffset := (1*direction + node.ExtendedNode.GPSCoordinateOffset) % len(m.GPXCoords)
			if nextOffset < 0 {
				nextOffset += len(m.GPXCoords) // Handle negative wrap-around
			}
			if strings.Contains(m.Travel, "point-to-point") {
				nextOffset = ZigzagIndex(tic+node.ExtendedNode.GPSCoordinateOffset, len(m.GPXCoords))
			}

			node.ExtendedNode.GPSCoordinateOffset = nextOffset
			node.Latitude = m.GPXCoords[nextOffset].Latitude
			node.Longitude = m.GPXCoords[nextOffset].Longitude

			scale := math.Cos(float64(node.Latitude) * math.Pi / 180.0)
			node.Latitude += r.Int31n(int32(fleet.LatLongAltGitter)*2) - int32(fleet.LatLongAltGitter) // +/- X
			node.Longitude += int32(float64(r.Int31n(int32(fleet.LatLongAltGitter)*2)-int32(fleet.LatLongAltGitter)) * scale)

			node.Altitude = m.GPXCoords[nextOffset].Altitude
			node.Precision = uint32(m.GPXCoords[nextOffset].Precision)
			f.position(idx, node)
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
