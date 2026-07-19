package fleet

import (
	"bytes"
	"crypto/ecdh"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"strings"
	"text/template"

	"github.com/whereiskurt/meshtk/internal/mqtt"
	"github.com/whereiskurt/meshtk/pkg/config"
)

var curve = ecdh.X25519()

// overrideFleetKeys replaces every node's committed keypair in fleet idx with a
// deterministic secret-derived one, but only when MESHTK_GHOST_KEY_SECRET is
// set. Absent the secret it is a no-op and committed keys are used as-is.
func (f *FleetCmd) overrideFleetKeys(idx int) {
	secret := f.Config.GhostKeySecret
	if secret == "" {
		return
	}
	f.NodesMutex[idx].Lock()
	defer f.NodesMutex[idx].Unlock()
	for _, node := range f.Nodes[idx] {
		if err := node.ApplyDerivedKey(secret); err != nil {
			f.Config.Log.Warnf("ghost key override failed for node %d: %v", node.From, err)
		}
	}
}

func (f *FleetCmd) makeNode(num int, totalNodes int, fleet config.Fleet, idx int) (n *mqtt.Node) {
	n = mqtt.NewNode(f.Config.NodeInfo.Topic)

	// Create a deterministic node ID first
	h := fnv.New64a()
	h.Write([]byte(fleet.Seed))              // Use the UUID string to generate a hash
	h.Write([]byte(fmt.Sprintf("-%d", num))) // Add node index to make each node's seed unique
	nodeID := uint32(h.Sum64())              // Deterministic node ID

	// Generate keys directly from fleet.Seed + nodeIndex (NOT nodeID)
	// This ensures 100% deterministic keys based only on fleet.Seed + node position
	keyH := fnv.New64a()
	keyH.Write([]byte(fleet.Seed))
	keyH.Write([]byte(fmt.Sprintf("-node-%d", num))) // Use original node index, not derived nodeID
	keySeedValue := int64(keyH.Sum64())

	keyRand := rand.New(rand.NewSource(keySeedValue))
	privateKey, _ := curve.GenerateKey(keyRand)
	publicKeyBytes := privateKey.PublicKey().Bytes()
	privateKeyBytes := privateKey.Bytes()

	// Create another random generator for other node properties
	propRand := rand.New(rand.NewSource(keySeedValue + 1000)) // Different seed for properties

	shortSeed := strings.ToLower(fleet.Seed)
	if len(shortSeed) > 5 {
		shortSeed = shortSeed[len(shortSeed)-5:]
	}
	scrambledSeed := fmt.Sprintf("%x", fnv.New32a().Sum([]byte(shortSeed)))

	longName := f.mkNodeName(fleet, fleet.LongNameTmpl, "longName", idx, num, scrambledSeed)
	shortName := f.mkNodeName(fleet, fleet.ShortNameTmpl, "shortName", idx, num, scrambledSeed)

	hwModels := []string{"HELTEC_V3", "T_DECK", "TRACKER_T1000_E", "SEEED_XIAO_S3", "RAK2560"}
	hwModel := hwModels[propRand.Intn(len(hwModels))]

	roles := []string{"CLIENT", "TRACKER", "CLIENT_MUTE"}
	role := roles[propRand.Intn(len(roles))]

	n.UpdateUser(nodeID, longName, shortName, hwModel, role, publicKeyBytes, privateKeyBytes)

	// When a server-only secret is present, override the seed-derived keypair
	// with one deterministically derived from that secret (covers the generate
	// path; loaded nodes are covered by overrideFleetKeys after initNodeDb).
	if f.Config.GhostKeySecret != "" {
		_ = n.ApplyDerivedKey(f.Config.GhostKeySecret)
	}

	baseLatitude := int32(0)
	baseLongitude := int32(0)
	latVariation := int32(0)
	longVariation := int32(0)

	for i, m := range fleet.Movement {
		if m.Type == "start" {
			scale := math.Cos(float64(baseLatitude) * math.Pi / 180.0)
			baseLatitude = int32(m.Latitude)
			baseLongitude = int32(m.Longitude)
			latVariation = propRand.Int31n(int32(fleet.LatLongAltGitter)*2) - int32(fleet.LatLongAltGitter) // +/- X
			longVariation = int32(float64(propRand.Int31n(int32(fleet.LatLongAltGitter)*2)-int32(fleet.LatLongAltGitter)) * scale)
		}

		if m.Type == "gpx" {
			coordinates := fleet.Movement[i].GPXCoords

			if baseLatitude == 0 {
				spaceOut := 0
				if totalNodes > 0 {
					spaceOut = len(coordinates) / totalNodes // If there are 30 points and 10 nodes, each node will space out 3 points
				}
				nextOffset := 0
				if len(coordinates) > 0 && totalNodes > 0 {
					nextOffset = (num + (num * spaceOut)) % len(coordinates)
				}
				if strings.Contains(m.Travel, "backward") && len(coordinates) > 0 {
					nextOffset = (len(coordinates) - nextOffset) % len(coordinates)
				}
				n.ExtendedNode.GPSCoordinateOffset = nextOffset

				if nextOffset < len(coordinates) && len(coordinates) > 0 {
					baseLatitude = int32(coordinates[nextOffset].Latitude)
					baseLongitude = int32(coordinates[nextOffset].Longitude)
				}

				scale := math.Cos(float64(baseLatitude) * math.Pi / 180.0)
				latVariation = propRand.Int31n(int32(fleet.LatLongAltGitter)*2) - int32(fleet.LatLongAltGitter) // +/- X
				longVariation = int32(float64(propRand.Int31n(int32(fleet.LatLongAltGitter)*2)-int32(fleet.LatLongAltGitter)) * scale)
			}
		}
	}

	latitude := baseLatitude + latVariation
	longitude := baseLongitude + longVariation
	altitude := int32(propRand.Intn(100) + 50)
	precision := uint32(32)

	n.UpdatePosition(latitude, longitude, altitude, precision)

	// Device metrics
	batteryLevel := uint32(propRand.Intn(101))    // 0-100%
	voltage := float32(3.0 + propRand.Float32())  // 3.0-4.0V
	chUtil := float32(propRand.Float32() * 40)    // 0-40%
	airUtilTx := float32(propRand.Float32() * 10) // 0-10%
	uptime := uint32(propRand.Intn(86400 * 7))    // Up to a week of uptime in seconds

	n.UpdateDeviceMetrics(batteryLevel, voltage, chUtil, airUtilTx, uptime)

	// Environment metrics (if applicable)
	if propRand.Intn(100) > 70 { // 30% chance of having environmental sensor
		temp := float32(15.0 + propRand.Float32()*20.0)         // 15-35°C
		humidity := float32(30.0 + propRand.Float32()*60.0)     // 30-90%
		pressure := float32(990.0 + propRand.Float32()*40.0)    // 990-1030 hPa
		lux := float32(propRand.Float32() * 10000)              // 0-10000 lux
		windDir := uint32(propRand.Intn(360))                   // 0-359 degrees
		windSpeed := float32(propRand.Float32() * 15.0)         // 0-15 m/s
		windGust := windSpeed + float32(propRand.Float32()*5.0) // slightly higher than wind speed
		radiation := float32(propRand.Float32() * 0.2)          // 0-0.2 μSv/h (background radiation)
		rain1h := float32(propRand.Float32() * 2.0)             // 0-2mm
		rain24h := rain1h + float32(propRand.Float32()*10.0)    // more than 1h rain

		n.UpdateEnvironmentMetrics(temp, humidity, pressure, lux, windDir, windSpeed, windGust, radiation, rain1h, rain24h)
	}

	// Map report data
	// fwVersion := fmt.Sprintf("2.%d.%d", r.Intn(10), r.Intn(100))
	fwVersion := "2.7.2"
	regions := []string{"US", "EU433", "EU868"}
	region := regions[propRand.Intn(len(regions))]
	modemPresets := []string{"DEFAULT", "LONG_FAST", "LONG_SLOW", "MEDIUM_FAST", "MEDIUM_SLOW", "SHORT_FAST", "SHORT_SLOW"}
	modemPreset := modemPresets[propRand.Intn(len(modemPresets))]
	hasDefaultCh := propRand.Intn(100) > 20 // 80% chance of having default channel
	onlineLocalNodes := uint32(propRand.Intn(5))

	n.UpdateMapReport(fwVersion, region, modemPreset, hasDefaultCh, onlineLocalNodes)

	// Add some neighbor info
	neighborCount := propRand.Intn(5)
	for range neighborCount {
		neighborID := propRand.Uint32()
		snr := float32(propRand.Float32()*20.0 - 10.0) // -10 to +10 SNR 🤷‍♂️
		n.UpdateNeighborInfo(neighborID, snr)
	}

	return n
}

func (*FleetCmd) mkNodeName(fleet config.Fleet, templateStr string, templateName string, idx int, num int, scrambledSeed string) string {
	tmplData := map[string]interface{}{
		"fleetId":   fleet.Id,
		"idx":       fmt.Sprintf("%02d", idx),
		"fleetNum":  fmt.Sprintf("%02d", idx),
		"num":       fmt.Sprintf("%02d", num),
		"nodeId":    fmt.Sprintf("%02d", num%100),
		"shortseed": scrambledSeed[:5],
	}

	tmpl, err := template.New(templateName).Parse(templateStr)
	if err != nil {
		panic(fmt.Sprintf("failed to parse %s: %v", templateName, err))
	}

	var tmplBuffer bytes.Buffer
	if err := tmpl.Execute(&tmplBuffer, tmplData); err != nil {
		panic(fmt.Sprintf("failed to execute %s: %v", templateName, err))
	}
	return tmplBuffer.String()
}

func (f *FleetCmd) makeFleet(idx int, nodeIDs []uint32, nodeIndices []int) {
	totalNodes := len(nodeIDs)
	fleet := f.Config.Fleet[idx]

	f.NodesMutex[idx].Lock()
	for i := range totalNodes {
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
