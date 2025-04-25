package fleet

import (
	"fmt"

	"github.com/whereiskurt/meshtk/internal/mqtt"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)

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

func (f *FleetCmd) behaviours(idx int, node *mqtt.Node, tic int) {
	tags := f.Config.Fleet[idx].BehaviourTag
	tagMap := make(map[string]bool)
	for _, tag := range tags {
		tagMap[tag] = true
	}

	// Make moves, and then tell folks.
	if tagMap["movement"] {
		f.movement(idx, node, tic)
	}
	if tagMap["nodeinfo"] {
		f.nodeinfo(idx, node)
	}
	if tagMap["position"] {
		f.position(idx, node)
	}
	if tagMap["sayhello"] {
		const ALL = 0xffffffff
		whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)
		f.MqttClient[idx].PublishMessageEncrypted(node.From, ALL, whoamiTopic, meshtastic.PortNum_TEXT_MESSAGE_APP, []byte("Hello world!"))
	}
}

func (f *FleetCmd) movement(idx int, node *mqtt.Node, tic int) {

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
