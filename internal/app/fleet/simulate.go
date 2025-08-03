package fleet

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"time"
)

func (f *FleetCmd) simulate(idx int) {
	fleet := f.Config.Fleet[idx]
	ramp := fleet.NodesPerRampInterval

	totalNodes := 0
	for r := range ramp {
		totalNodes += ramp[r]
	}
	randIndices := rand.Perm(totalNodes)
	nodeIDs := make([]uint32, totalNodes)

	// Load GPX coordinates for any nodes that might need access to them
	for m := range fleet.Movement {
		if fleet.Movement[m].Type == "gpx" && fleet.Movement[m].GPXFile != "" {
			//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Loading GPX Fleet[%d] with %d nodes...\n", idx, totalNodes)))
			gpxFile := fleet.Movement[m].GPXFile
			coordinates := f.GPXCoords(gpxFile)
			fleet.Movement[m].GPXCoords = coordinates
			//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Done! Loaded GPX Fleet[%d] with %d nodes.\n", idx, len(coordinates))))
		}
	}

	// Initialize or reuse fleet nodes
	if len(f.Nodes[idx]) == 0 {
		// No nodes exist, create them
		//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Creating Fleet[%d] with %d nodes...\n", idx, totalNodes)))
		f.makeFleet(idx, nodeIDs, randIndices)
	} else {
		// Nodes already exist from database, reuse them
		//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Reusing Fleet[%d] with %d existing nodes...\n", idx, len(f.Nodes[idx]))))
		// Check if we have the exact nodes we need based on deterministic IDs
		needsCreation := false
		for i := 0; i < totalNodes; i++ {
			// Calculate expected node ID
			h := fnv.New64a()
			h.Write([]byte(fleet.Seed))
			h.Write([]byte(fmt.Sprintf("-%d", i)))
			expectedNodeID := uint32(h.Sum64())

			if _, exists := f.Nodes[idx][expectedNodeID]; !exists {
				needsCreation = true
				break
			}
		}

		if needsCreation {
			// Some nodes are missing, recreate the fleet
			//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Some nodes missing, recreating Fleet[%d]...\n", idx)))
			f.makeFleet(idx, nodeIDs, randIndices)
		} else {
			// All nodes exist, populate nodeIDs array
			for i := 0; i < totalNodes; i++ {
				h := fnv.New64a()
				h.Write([]byte(fleet.Seed))
				h.Write([]byte(fmt.Sprintf("-%d", i)))
				nodeID := uint32(h.Sum64())
				nodeIDs[randIndices[i]] = nodeID
			}
		}
	}
	f.rampUp(idx, nodeIDs, randIndices)
	f.rampSteady(idx, nodeIDs, randIndices)
	f.rampDown(idx, nodeIDs, randIndices)
}

func (f *FleetCmd) rampUp(idx int, nodeIDs []uint32, randIndices []int) {
	fleet := f.Config.Fleet[idx]

	ramp := fleet.NodesPerRampInterval
	rampLen := len(ramp)
	rampUpMs := fleet.RampUpSecs * 1000
	rampIntervalMs := rampUpMs / rampLen
	totalNodes := 0

	if f.Config.Fleet[idx].Distribution == "uniform" {
		//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Uniformly adding %d nodes over %d seconds\n", idx, len(nodeIDs), fleet.RampUpSecs)))

		for r := range rampLen {
			newNodes := ramp[r]
			nodeEveryMs := rampIntervalMs / newNodes
			if newNodes > 0 {
				//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Ramp[%d][%d]: Adding %d nodes in %d ms (node every %d ms)\n", idx, r, newNodes, rampIntervalMs, nodeEveryMs)))
				for i := range newNodes {
					nodeIndex := totalNodes + i
					node := f.Nodes[idx][nodeIDs[randIndices[nodeIndex%len(randIndices)]]]
					f.publishNodeInfo(idx, node)
					f.publishPosition(idx, node, false)
					time.Sleep(time.Duration(nodeEveryMs) * time.Millisecond)
				}
				totalNodes += newNodes
			}
		}
		//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Ramp-up complete. Successfully added %d nodes.\n", idx, totalNodes)))

	}
}

func (f *FleetCmd) rampSteady(idx int, nodeIDs []uint32, randIndices []int) {

	fleet := f.Config.Fleet[idx]
	totalSteadyStateSecs := fleet.RampSteadySecs
	// maxTics := totalSteadyStateSecs / fleet.BehaviourSecs
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
			totalNodes := fleet.NodesPerSteadyInterval[tic%len(fleet.NodesPerSteadyInterval)]
			// f.Config.Stdout.Write([]byte(fmt.Sprintf("⏱️  Fleet[%d]: Running steady state: tic %d/%d\n", idx, tic+1, maxTics)))
			for range totalNodes {
				nodeID := nodeIDs[randIndices[nodeOffset%len(randIndices)]]
				f.behaviours(idx, f.Nodes[idx][nodeID], tic)
				nodeOffset++
			}
			// f.Config.Stdout.Write([]byte(fmt.Sprintf("⏱️  Fleet[%d]: Sleeping for %d seconds...\n", idx, fleet.BehaviourSecs)))
			time.Sleep(time.Duration(fleet.BehaviourSecs) * time.Second) // Adjust sleep duration as needed
			tic++
		}
	}

	//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Steady state complete.\n", idx)))
}

func (f *FleetCmd) rampDown(idx int, nodeIDs []uint32, randIndices []int) {
	//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Ramp down initiated.\n", idx)))
}
