package fleet

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"

	"github.com/whereiskurt/meshtk/internal/mqtt"
	"github.com/whereiskurt/meshtk/pkg/config"
)

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

	hwModels := []string{"HELTEC_V3", "T_DECK", "TRACKER_T1000_E", "SEEED_XIAO_S3", "RAK2560"}
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
				baseLatitude = int32(coordinates[num%len(coordinates)].Latitude)
				baseLongitude = int32(coordinates[num%len(coordinates)].Longitude)
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
