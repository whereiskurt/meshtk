package fleet

import (
	"crypto/ecdh"
	"fmt"

	"math/rand"

	"time"

	"github.com/whereiskurt/meshtk/internal/mqtt"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)

var curve = ecdh.X25519()

func (f *FleetCmd) simulate(idx int) {
	fleet := f.Config.Fleet[idx]
	ramp := fleet.NodesPerRampInterval

	totalNodes := 0
	for r := range ramp {
		totalNodes += ramp[r]
	}
	randIndices := rand.Perm(totalNodes)
	nodeIDs := make([]uint32, totalNodes)

	// Initialize or reuse fleet nodes
	if len(f.Nodes[idx]) < totalNodes {
		f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Creating Fleet[%d] with %d nodes...\n", idx, totalNodes)))
		f.makeFleet(idx, nodeIDs, randIndices)
	} else {
		f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Reusing Fleet[%d] with %d nodes...\n", idx, len(f.Nodes[idx]))))
		for i := range totalNodes {
			node := f.makeNode(i, fleet, idx)
			f.Nodes[idx][node.From] = node
			nodeIDs[randIndices[i]] = node.From
		}
	}
	f.rampUp(idx, nodeIDs, randIndices)
	f.steadyState(idx, nodeIDs, randIndices)
	f.rampDown(idx, nodeIDs, randIndices)
}

func (f *FleetCmd) steadyState(idx int, nodeIDs []uint32, randIndices []int) {

	fleet := f.Config.Fleet[idx]
	totalSteadyStateMs := fleet.RampSteadySecs * 1000
	steadyStateIntervalMs := totalSteadyStateMs / len(fleet.NodesPerSteadyInterval)

	offset := 0
	for i := range fleet.NodesPerSteadyInterval {
		f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Steady state interval %d with %d nodes...\n", idx, i, fleet.NodesPerSteadyInterval[i])))

		nodesThisInterval := fleet.NodesPerSteadyInterval[i]
		nodeEveryMs := steadyStateIntervalMs / nodesThisInterval

		for r := range nodesThisInterval {
			nodeIndex := offset + r
			node := f.Nodes[idx][nodeIDs[randIndices[nodeIndex%len(f.Nodes[idx])]]]
			f.behaviours(idx, node)
			time.Sleep(time.Duration(nodeEveryMs) * time.Millisecond)
		}
		offset += fleet.NodesPerSteadyInterval[i]
	}

	f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Steady state complete.\n", idx)))
}

func (f *FleetCmd) rampUp(idx int, nodeIDs []uint32, randIndices []int) {
	fleet := f.Config.Fleet[idx]

	ramp := fleet.NodesPerRampInterval
	rampLen := len(ramp)
	rampUpMs := fleet.RampUpSecs * 1000
	rampIntervalMs := rampUpMs / rampLen
	totalNodes := 0

	if f.Config.Fleet[idx].Distribution == "uniform" {
		f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Uniformly adding %d nodes over %d seconds\n", idx, len(nodeIDs), fleet.RampUpSecs)))

		for r := range rampLen {
			newNodes := ramp[r]
			nodeEveryMs := rampIntervalMs / newNodes
			if newNodes > 0 {
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Ramp[%d]: Adding %d nodes in %d ms (node every %d ms)\n", r, newNodes, rampIntervalMs, nodeEveryMs)))
				for i := range newNodes {
					nodeIndex := totalNodes + i
					if nodeIndex < len(nodeIDs) {
						node := f.Nodes[idx][nodeIDs[randIndices[nodeIndex]]]
						f.nodeinfo(idx, node)
						f.position(idx, node)
						time.Sleep(time.Duration(nodeEveryMs) * time.Millisecond)
					}
				}
				totalNodes += newNodes
			}
		}
		f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Ramp-up complete. Successfully added %d nodes.\n", idx, totalNodes)))

	}
}

func (f *FleetCmd) rampDown(idx int, nodeIDs []uint32, randIndices []int) {
	f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Ramp down initiated.\n", idx)))
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

func (f *FleetCmd) behaviours(idx int, node *mqtt.Node) {

	tags := f.Config.Fleet[idx].BehaviourTag
	tagMap := make(map[string]bool)
	for _, tag := range tags {
		tagMap[tag] = true
	}

	if tagMap["announce"] {
		f.nodeinfo(idx, node)
		f.position(idx, node)
	}
	if tagMap["sayhello"] {
		const ALL = 0xffffffff
		whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)
		f.MqttClient[idx].PublishMessageEncrypted(node.From, ALL, whoamiTopic, meshtastic.PortNum_TEXT_MESSAGE_APP, []byte("Hello world!"))
	}
	if tagMap["movement"] {
		f.position(idx, node)
	}

}
