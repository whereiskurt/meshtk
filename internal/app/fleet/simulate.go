package fleet

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"time"

	"github.com/whereiskurt/meshtk/internal/mqtt"

	"github.com/whereiskurt/meshtk/pkg/config"
)

func (f *FleetCmd) makeNode(offset int, fleet config.Fleet) *mqtt.Node {

	// Create a hash from the UUID string seed to get a deterministic int64 seed
	h := fnv.New64a()
	h.Write([]byte(fleet.Seed))                 // Use the UUID string to generate a hash
	h.Write([]byte(fmt.Sprintf("-%d", offset))) // Add node index to make each node's seed unique
	seedValue := int64(h.Sum64())

	// Create a new seeded random source based on the hash of fleet seed and node index
	r := rand.New(rand.NewSource(seedValue))

	// Create a basic node with a topic
	topic := fmt.Sprintf("msh/2/%08x/c/stat", uint32(r.Uint32()))
	n := mqtt.NewNode(topic)

	// Generate node ID (from)
	nodeID := r.Uint32()

	// Set up user data
	// Extract a short version of the seed for display purposes by taking the last 8 characters
	shortSeed := fleet.Seed
	if len(shortSeed) > 8 {
		shortSeed = shortSeed[len(shortSeed)-8:]
	}
	longName := fmt.Sprintf("Fleet-%s-Node-%d", shortSeed, offset)
	shortName := fmt.Sprintf("F%s%d", shortSeed[:4], offset%100)

	// Select a random hardware model
	hwModels := []string{"TBEAM", "TBEAM0.7", "TBEAM1.1", "HELTEC", "HELTEC_V1", "HELTEC_V2", "RAK4631", "MESHTASTIC_DIY_V1"}
	hwModel := hwModels[r.Intn(len(hwModels))]

	// Select a random role
	roles := []string{"CLIENT", "ROUTER", "REPEATER", "SENSOR", "TRACKER", "POWER"}
	role := roles[r.Intn(len(roles))]

	// Update user info
	n.UpdateUser(nodeID, longName, shortName, hwModel, role, nil)

	// Position data - use NodeInfo lat/long as a base with some randomness
	// Converting from float to int32 as expected by the Node structure
	baseLatitude := int32(f.Config.NodeInfo.Latitude)
	baseLongitude := int32(f.Config.NodeInfo.Longitude)

	// Add some randomness (within about 1km)
	latVariation := r.Int31n(20000) - 10000  // +/- 0.001 degrees (~100m)
	longVariation := r.Int31n(20000) - 10000 // +/- 0.001 degrees (~100m at equator)

	latitude := baseLatitude + latVariation
	longitude := baseLongitude + longVariation
	altitude := int32(r.Intn(100) + 50)  // 50-150m
	precision := uint32(r.Intn(16) + 16) // precision between 16-32

	n.UpdatePosition(latitude, longitude, altitude, precision)

	// Device metrics
	batteryLevel := uint32(r.Intn(101))    // 0-100%
	voltage := float32(3.0 + r.Float32())  // 3.0-4.0V
	chUtil := float32(r.Float32() * 40)    // 0-40%
	airUtilTx := float32(r.Float32() * 10) // 0-10%
	uptime := uint32(r.Intn(86400 * 7))    // Up to a week of uptime in seconds

	n.UpdateDeviceMetrics(batteryLevel, voltage, chUtil, airUtilTx, uptime)

	// Environment metrics (if applicable)
	if r.Intn(100) > 70 { // 30% chance of having environmental sensor
		temp := float32(15.0 + r.Float32()*20.0)         // 15-35°C
		humidity := float32(30.0 + r.Float32()*60.0)     // 30-90%
		pressure := float32(990.0 + r.Float32()*40.0)    // 990-1030 hPa
		lux := float32(r.Float32() * 10000)              // 0-10000 lux
		windDir := uint32(r.Intn(360))                   // 0-359 degrees
		windSpeed := float32(r.Float32() * 15.0)         // 0-15 m/s
		windGust := windSpeed + float32(r.Float32()*5.0) // slightly higher than wind speed
		radiation := float32(r.Float32() * 0.2)          // 0-0.2 μSv/h (background radiation)
		rain1h := float32(r.Float32() * 2.0)             // 0-2mm
		rain24h := rain1h + float32(r.Float32()*10.0)    // more than 1h rain

		n.UpdateEnvironmentMetrics(temp, humidity, pressure, lux, windDir, windSpeed, windGust, radiation, rain1h, rain24h)
	}

	// Map report data
	fwVersion := fmt.Sprintf("2.%d.%d", r.Intn(10), r.Intn(100))
	regions := []string{"US", "EU433", "EU868", "CN", "JP", "ANZ", "KR", "TW", "RU", "IN", "NZ", "TH"}
	region := regions[r.Intn(len(regions))]
	modemPresets := []string{"DEFAULT", "LONG_FAST", "LONG_SLOW", "MEDIUM_FAST", "MEDIUM_SLOW", "SHORT_FAST", "SHORT_SLOW"}
	modemPreset := modemPresets[r.Intn(len(modemPresets))]
	hasDefaultCh := r.Intn(100) > 20 // 80% chance of having default channel
	onlineLocalNodes := uint32(r.Intn(5))

	n.UpdateMapReport(fwVersion, region, modemPreset, hasDefaultCh, onlineLocalNodes)

	// Add some neighbor info
	neighborCount := r.Intn(5)
	for i := 0; i < neighborCount; i++ {
		neighborID := r.Uint32()
		snr := float32(r.Float32()*20.0 - 10.0) // -10 to +10 SNR
		n.UpdateNeighborInfo(neighborID, snr)
	}

	return n
}

func (f *FleetCmd) StartSimulation(idx int) {
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
		f.makeFleetNodes(idx, nodeIDs, randIndices)
	} else {
		f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Reusing Fleet[%d] with %d nodes...\n", idx, len(f.Nodes[idx]))))
		for i := range totalNodes {
			node := f.makeNode(i, fleet)
			f.Nodes[idx][node.From] = node
			nodeIDs[randIndices[i]] = node.From
		}
	}

	f.rampUp(idx, nodeIDs, randIndices)

}

func (f *FleetCmd) rampUp(idx int, nodeIDs []uint32, randIndices []int) {
	fleet := f.Config.Fleet[idx]

	ramp := fleet.NodesPerRampInterval
	rampLen := len(ramp)
	rampUpMs := fleet.RampUpSecs * 1000
	rampIntervalMs := rampUpMs / rampLen

	if f.Config.Fleet[idx].Distribution == "uniform" {
		f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Uniformly adding %d nodes over %d seconds\n", idx, len(nodeIDs), fleet.RampUpSecs)))

		nodesAnnounced := 0
		for r := 0; r < rampLen; r++ {
			newNodes := ramp[r]
			nodeEveryMs := rampIntervalMs / newNodes
			if newNodes > 0 {
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Ramp[%d]: Adding %d nodes in %d ms (node every %d ms)\n", r, newNodes, rampIntervalMs, nodeEveryMs)))
				for i := 0; i < newNodes; i++ {
					nodeIndex := nodesAnnounced + i
					if nodeIndex < len(nodeIDs) {
						node := f.Nodes[idx][nodeIDs[randIndices[nodeIndex]]]
						f.announce(node)
						time.Sleep(time.Duration(nodeEveryMs) * time.Millisecond)
					}
				}
				nodesAnnounced += newNodes
			}
		}
		f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Ramp-up complete. Successfully added %d nodes.\n", idx, nodesAnnounced)))
	}
}

func (f *FleetCmd) makeFleetNodes(idx int, nodeIDs []uint32, nodeIndices []int) {
	totalNodes := len(nodeIDs)
	fleet := f.Config.Fleet[idx]

	f.NodesMutex[idx].Lock()
	for i := 0; i < totalNodes; i++ {
		node := f.makeNode(i, fleet)
		f.Nodes[idx][node.From] = node
		nodeIDs[nodeIndices[i]] = node.From
	}
	f.NodesMutex[idx].Unlock()
	f.flushNodeDb(idx)
}

func (f *FleetCmd) announce(node *mqtt.Node) {
	f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Announcing node %s\n", node.FromStr)))
	// Publish the node's status via MQTT
	// This is where you would actually publish the node's data to the MQTT broker
}

func (f *FleetCmd) initNodeDb(fleetIdx int) {
	f.Nodes[fleetIdx].LoadFile(f.Config.Fleet[fleetIdx].NodeDbPath)
	f.Config.Stdout.Write([]byte(fmt.Sprintf("💾 Loaded %d nodes from %s\n", len(f.Nodes[fleetIdx]), f.Config.Fleet[fleetIdx].NodeDbPath)))
}

func (f *FleetCmd) flushNodeDb(fleetIdx int) {
	f.NodesMutex[fleetIdx].Lock()
	f.Nodes[fleetIdx].WriteFile(f.Config.Fleet[fleetIdx].NodeDbPath)
	f.NodesMutex[fleetIdx].Unlock()
}
