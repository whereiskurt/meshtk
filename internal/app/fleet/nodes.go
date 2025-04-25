package fleet

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"

	"github.com/whereiskurt/meshtk/internal/mqtt"
	"github.com/whereiskurt/meshtk/pkg/config"
)

func (f *FleetCmd) makeNode(num int, totalNodes int, fleet config.Fleet, idx int) (n *mqtt.Node) {
	n = mqtt.NewNode(f.Config.NodeInfo.Topic)

	// Create a hash from the UUID string seed to get a deterministic int64 seed
	h := fnv.New64a()
	h.Write([]byte(fleet.Seed))              // Use the UUID string to generate a hash
	h.Write([]byte(fmt.Sprintf("-%d", num))) // Add node index to make each node's seed unique
	seedValue := int64(h.Sum64())

	r := rand.New(rand.NewSource(seedValue))

	nodeID := r.Uint32()
	privateKey, _ := curve.GenerateKey(r)
	publicKeyBytes := privateKey.PublicKey().Bytes()
	privateKeyBytes := privateKey.Bytes()

	shortSeed := strings.ToLower(fleet.Seed)
	if len(shortSeed) > 5 {
		shortSeed = shortSeed[len(shortSeed)-5:]
	}
	scrambledSeed := fmt.Sprintf("%x", fnv.New32a().Sum([]byte(shortSeed)))
	longName := fmt.Sprintf("mtk%02d%02d-%s", idx, num%100, scrambledSeed[:5])
	shortName := fmt.Sprintf("M%02d%02d", idx, num%100)

	hwModels := []string{"HELTEC_V3", "T_DECK", "TRACKER_T1000_E", "SEEED_XIAO_S3", "RAK2560"}
	hwModel := hwModels[r.Intn(len(hwModels))]

	roles := []string{"CLIENT", "TRACKER", "CLIENT_MUTE"}
	role := roles[r.Intn(len(roles))]

	n.UpdateUser(nodeID, longName, shortName, hwModel, role, publicKeyBytes, privateKeyBytes)

	baseLatitude := int32(0)
	baseLongitude := int32(0)
	latVariation := int32(0)
	longVariation := int32(0)

	for i, m := range fleet.Movement {
		if m.Type == "start" {
			baseLatitude = int32(m.Latitude)
			baseLongitude = int32(m.Longitude)
			latVariation = r.Int31n(20000) - 10000
			longVariation = r.Int31n(20000) - 10000
		}

		if m.Type == "gpx" {
			//TODO: Move this out! Doesn't need to happen every makenode!
			coordinates := f.GPXCoords(m.GPXFile)
			fleet.Movement[i].GPXCoords = coordinates
			if baseLatitude == 0 {
				spaceOut := len(coordinates) / totalNodes // If there are 30 points and 10 nodes, each node will space out 3 points
				baseLatitude = int32(coordinates[(num*spaceOut)%len(coordinates)].Latitude)
				baseLongitude = int32(coordinates[(num*spaceOut)%len(coordinates)].Longitude)
			}
		}
	}

	latitude := baseLatitude + latVariation
	longitude := baseLongitude + longVariation
	altitude := int32(r.Intn(100) + 50)
	precision := uint32(32)

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
		snr := float32(r.Float32()*20.0 - 10.0) // -10 to +10 SNR 🤷‍♂️
		n.UpdateNeighborInfo(neighborID, snr)
	}

	return n
}
func (f *FleetCmd) makeFleet(idx int, nodeIDs []uint32, nodeIndices []int) {
	totalNodes := len(nodeIDs)
	fleet := f.Config.Fleet[idx]

	f.NodesMutex[idx].Lock()
	for i := 0; i < totalNodes; i++ {
		node := f.makeNode(i, totalNodes, fleet, idx)
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
