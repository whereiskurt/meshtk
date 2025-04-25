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
			node := f.makeNode(i, totalNodes, fleet, idx)
			f.Nodes[idx][node.From] = node
			nodeIDs[randIndices[i]] = node.From
		}
	}
	f.rampUp(idx, nodeIDs, randIndices)
	f.rampSteady(idx, nodeIDs, randIndices)
	f.rampDown(idx, nodeIDs, randIndices)
}

func (f *FleetCmd) rampSteady(idx int, nodeIDs []uint32, randIndices []int) {

	fleet := f.Config.Fleet[idx]
	totalSteadyStateSecs := fleet.RampSteadySecs
	maxTics := totalSteadyStateSecs / fleet.BehaviourSecs
	timer := time.NewTimer(time.Duration(totalSteadyStateSecs) * time.Second)
	defer timer.Stop()
	tic := 0
	nodeOffset := 0

TIMER:
	for {
		select {
		case <-timer.C:
			f.Config.Stdout.Write([]byte(fmt.Sprintf("⏱️ Fleet[%d]: Steady state timer expired.\n", idx)))
			break TIMER
		default:

			totalNodes := fleet.NodesPerRampInterval[tic%len(fleet.NodesPerRampInterval)]
			f.Config.Stdout.Write([]byte(fmt.Sprintf("⏱️  Fleet[%d]: Running steady-state behaviours ...\n", idx)))
			for range totalNodes {
				nodeID := nodeIDs[randIndices[nodeOffset%len(randIndices)]]
				f.behaviours(idx, f.Nodes[idx][nodeID], tic)
				nodeOffset++
			}
			f.Config.Stdout.Write([]byte(fmt.Sprintf("⏱️  Fleet[%d]: Steady state: tic %d/%d\n", idx, tic, maxTics)))
			f.Config.Stdout.Write([]byte(fmt.Sprintf("⏱️  Fleet[%d]: Sleeping for %d seconds...\n", idx, fleet.BehaviourSecs)))
			time.Sleep(time.Duration(fleet.BehaviourSecs) * time.Second) // Adjust sleep duration as needed
			tic++
		}
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
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Ramp[%d][%d]: Adding %d nodes in %d ms (node every %d ms)\n", idx, r, newNodes, rampIntervalMs, nodeEveryMs)))
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

func (f *FleetCmd) movement(idx int, node *mqtt.Node, tic int) {

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
