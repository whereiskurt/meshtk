package fleet

import (
	"crypto/ecdh"
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/whereiskurt/meshtk/internal/mqtt"
	"github.com/whereiskurt/meshtk/pkg/config"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)

// Define GPX file structure
type GPX struct {
	XMLName xml.Name `xml:"gpx"`
	Trk     []Track  `xml:"trk"`
}

type Track struct {
	Name    string     `xml:"name"`
	TrkSegs []TrackSeg `xml:"trkseg"`
}

type TrackSeg struct {
	TrkPts []TrackPoint `xml:"trkpt"`
}

type TrackPoint struct {
	Lat  float64 `xml:"lat,attr"`
	Lon  float64 `xml:"lon,attr"`
	Ele  float64 `xml:"ele"`
	Time string  `xml:"time"`
}

// Coordinate represents a latitude/longitude pair
type Coordinate struct {
	Latitude  int32
	Longitude int32
	Altitude  int32
}

var curve = ecdh.X25519()

// GPXCoords reads a GPX file and returns an array of coordinates
func (f *FleetCmd) GPXCoords(gpxFilePath string) []Coordinate {
	f.Config.Log.Infof("Reading GPX file: %s", gpxFilePath)

	// Read GPX file
	xmlFile, err := os.Open(gpxFilePath)
	if err != nil {
		f.Config.Log.Errorf("Failed to open GPX file: %s, error: %v", gpxFilePath, err)
		return []Coordinate{}
	}
	defer xmlFile.Close()

	byteValue, err := io.ReadAll(xmlFile)
	if err != nil {
		f.Config.Log.Errorf("Failed to read GPX file: %s, error: %v", gpxFilePath, err)
		return []Coordinate{}
	}

	// Parse XML
	var gpx GPX
	err = xml.Unmarshal(byteValue, &gpx)
	if err != nil {
		f.Config.Log.Errorf("Failed to parse GPX file: %s, error: %v", gpxFilePath, err)
		return []Coordinate{}
	}

	// Extract coordinates
	var coordinates []Coordinate
	for _, track := range gpx.Trk {
		for _, segment := range track.TrkSegs {
			for _, point := range segment.TrkPts {
				// Convert to int32 format used by the application (multiplied by 10^7)
				lat := int32(point.Lat * 10000000)
				lon := int32(point.Lon * 10000000)
				alt := int32(point.Ele)

				coordinates = append(coordinates, Coordinate{
					Latitude:  lat,
					Longitude: lon,
					Altitude:  alt,
				})
			}
		}
	}

	return coordinates
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
			node := f.makeNode(i, fleet, idx)
			f.Nodes[idx][node.From] = node
			nodeIDs[randIndices[i]] = node.From
		}
	}
	f.rampUp(idx, nodeIDs, randIndices)
	f.steadyState(idx, nodeIDs, randIndices)
}

func (f *FleetCmd) announce(idx int, node *mqtt.Node) {
	const ALL = 0xffffffff

	whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)

	hwModelNumber, ok := meshtastic.HardwareModel_value[node.HwModel]
	if !ok {
		f.Config.Log.Warnf("⚠️ Unknown hardware model: %s. Defaulting to 0.\n", node.HwModel)
		hwModelNumber = 0
	}
	f.MqttClient[idx].PublishNodeInfo(node.From, ALL, whoamiTopic, node.LongName, node.ShortName, meshtastic.HardwareModel(hwModelNumber), meshtastic.Config_DeviceConfig_CLIENT)

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
		f.announce(idx, node)
	}
	if tagMap["sayhello"] {
		const ALL = 0xffffffff
		whoamiTopic := fmt.Sprintf("%s/!%08x", f.Config.NodeInfo.Topic, node.From)
		f.MqttClient[idx].PublishMessageEncrypted(node.From, ALL, whoamiTopic, meshtastic.PortNum_TEXT_MESSAGE_APP, []byte("Hello world!"))
	}
	if tagMap["movement"] {
	}

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
						f.announce(idx, node)
						time.Sleep(time.Duration(nodeEveryMs) * time.Millisecond)
					}
				}
				totalNodes += newNodes
			}
		}
		f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Ramp-up complete. Successfully added %d nodes.\n", idx, totalNodes)))

	}
}

func (f *FleetCmd) makeNode(num int, fleet config.Fleet, idx int) *mqtt.Node {
	// Create a hash from the UUID string seed to get a deterministic int64 seed
	h := fnv.New64a()
	h.Write([]byte(fleet.Seed))              // Use the UUID string to generate a hash
	h.Write([]byte(fmt.Sprintf("-%d", num))) // Add node index to make each node's seed unique
	seedValue := int64(h.Sum64())

	// Create a new seeded random source based on the hash of fleet seed and node index
	r := rand.New(rand.NewSource(seedValue))

	// Create a basic node with a seen by topic
	n := mqtt.NewNode(f.Config.NodeInfo.Topic)

	privateKey, _ := curve.GenerateKey(r)
	publicKeyBytes := privateKey.PublicKey().Bytes()
	privateKeyBytes := privateKey.Bytes()

	// Generate node ID (from)
	nodeID := r.Uint32()

	shortSeed := strings.ToLower(fleet.Seed)
	if len(shortSeed) > 5 {
		shortSeed = shortSeed[len(shortSeed)-5:]
	}
	scrambledSeed := fmt.Sprintf("%x", fnv.New32a().Sum([]byte(shortSeed)))
	longName := fmt.Sprintf("mtk%s%02d%02d", scrambledSeed[:5], num%100, idx)
	shortName := fmt.Sprintf("MK%02d", num%100)

	hwModels := []string{"HELTEC_V3", "TRACKER_T1000_E", "SEEED_XIAO_S3"}
	hwModel := hwModels[r.Intn(len(hwModels))]

	roles := []string{"CLIENT", "TRACKER", "CLIENT_MUTE"}
	role := roles[r.Intn(len(roles))]

	n.UpdateUser(nodeID, longName, shortName, hwModel, role, publicKeyBytes, privateKeyBytes)

	startIndex := -1
	gpxIndex := -1

	baseLatitude := int32(0)
	baseLongitude := int32(0)

	for i, movement := range fleet.Movement {
		if movement.Type == "start" {
			startIndex = i
			baseLatitude = int32(fleet.Movement[startIndex].Latitude)
			baseLongitude = int32(fleet.Movement[startIndex].Longitude)
		}
		if movement.Type == "gpx" {
			gpxIndex = i
			coordinates := f.GPXCoords(fleet.Movement[gpxIndex].GPXFile)
			f.Config.Log.Tracef("Extracted %d coordinates from GPX file", len(coordinates))
			if startIndex == -1 {
				baseLatitude = int32(coordinates[0].Latitude)
				baseLongitude = int32(coordinates[0].Longitude)
			}
		}
	}
	latVariation := r.Int31n(20000) - 10000  // +/- 0.001 degrees (~100m)
	longVariation := r.Int31n(20000) - 10000 // +/- 0.001 degrees (~100m at equator)

	latitude := baseLatitude + latVariation
	longitude := baseLongitude + longVariation
	altitude := int32(r.Intn(100) + 50) // 50-150m
	// precision := uint32(r.Intn(16) + 16) // precision between 16-32
	precision := uint32(32) // precision between 16-32

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
	regions := []string{"US", "EU433", "EU868"}
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
func (f *FleetCmd) makeFleet(idx int, nodeIDs []uint32, nodeIndices []int) {
	totalNodes := len(nodeIDs)
	fleet := f.Config.Fleet[idx]

	f.NodesMutex[idx].Lock()
	for i := 0; i < totalNodes; i++ {
		node := f.makeNode(i, fleet, idx)
		f.Nodes[idx][node.From] = node
		nodeIDs[nodeIndices[i]] = node.From
	}
	f.NodesMutex[idx].Unlock()
	f.flushNodeDb(idx)
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
