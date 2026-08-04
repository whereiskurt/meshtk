package nodeinfo

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"

	"github.com/spf13/cobra"
	"github.com/whereiskurt/meshtk/internal/app/help"
	internal "github.com/whereiskurt/meshtk/internal/mqtt"
	"github.com/whereiskurt/meshtk/pkg/config"
	"github.com/whereiskurt/meshtk/pkg/network"
)

type NodeInfoCmd struct {
	Nodes      internal.NodeDB
	NodesMutex sync.Mutex
	// PubKeys retains the last public key learned for each node id, keyed by
	// node number. Unlike Nodes it is never pruned, so a node's pubkey survives
	// the prune-then-recreate churn (a brief publishing gap prunes the node,
	// and the next POSITION packet recreates it without a pubkey). Guarded by
	// NodesMutex. Restored into freshly-recreated nodes before every write.
	PubKeys    map[uint32]string
	Config     *config.Config
	MqttClient *internal.MqttClient
	CmdOutput  struct {
		WasSuccess bool
	}
}

func NewNodeInfo(c *config.Config) (n *NodeInfoCmd) {
	n = new(NodeInfoCmd)
	n.Config = c
	n.Nodes = make(internal.NodeDB)
	n.PubKeys = make(map[uint32]string)

	return n
}

// restorePubKeys fills in any node missing a public key from the retained
// PubKeys store. Caller must hold NodesMutex.
func (n *NodeInfoCmd) restorePubKeys() {
	for id, node := range n.Nodes {
		if node.PubKey == "" {
			if pk, ok := n.PubKeys[id]; ok && pk != "" {
				node.PubKey = pk
			}
		}
	}
}

func (n *NodeInfoCmd) Help(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	fmt.Fprintln(n.Config.Stdout, help.NodeInfoHelp(n.Config))
}

func (n *NodeInfoCmd) Announce(cmd *cobra.Command, argz []string) {
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("NodeInfoCmd.Announce")
	n.Config.Log.Tracef("%+v", n.Config)

	n.initNodeDb()

	topics := n.Config.NodeInfo.SubscribedTopics

	n.MqttClient = internal.NewMqttClient(n.Config, &n.Nodes, n.NodeHandler)

	n.MqttClient.ConnectAndListen(topics)

	if n.Config.NodeInfo.BroadcastOnLoad {
		n.Config.Stdout.Write([]byte("🚀 Doing a Broadcasting onLoad()"))
		n.DoBroadcast()
	}

	go func() {
		if int(n.Config.NodeInfo.BroadcastIntervalSec) < 1 {
			n.Config.Log.Trace("Broadcast interval is set to 0, not broadcasting")
			return
		}
		n.Config.Stdout.Write([]byte(fmt.Sprintf(" with %d second rebroadcast...\n", n.Config.NodeInfo.BroadcastIntervalSec)))
		ticker := time.NewTicker(time.Duration(int(n.Config.NodeInfo.BroadcastIntervalSec)) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			n.Config.Stdout.Write([]byte("."))
			n.DoBroadcast()
		}
	}()

	if int(n.Config.NodeInfo.BroadcastIntervalSec) > 1 {
		n.MqttClient.WaitUntilKill()
	}
	n.Config.Stdout.Write([]byte("\n✅ Cleanly exiting ...\n"))

	n.flushNodeDb()
	n.CmdOutput.WasSuccess = true
}

func (n *NodeInfoCmd) DoBroadcast() {
	fromMeshHex := n.Config.NodeInfo.ClientId
	fromUint, err := strconv.ParseUint(fromMeshHex[1:], 16, 32) // We start at 1 to skip the '!' base16 for 4 bytes
	if err != nil {
		n.Config.Log.Errorf("failed to parse hex id: %v", err)
		return
	}
	from := uint32(fromUint)

	var lat int32 = int32(n.Config.NodeInfo.Latitude)
	var lng int32 = int32(n.Config.NodeInfo.Longitude)
	var alt int32 = int32(n.Config.NodeInfo.Altitude)
	var prec uint32 = uint32(n.Config.NodeInfo.Precision)

	const ALL = 0xffffffff

	whoamiTopic := fmt.Sprintf("%s/!%08x", n.Config.NodeInfo.Topic, from)

	n.Config.Log.Tracef("Broadcasting to %s", whoamiTopic)
	n.Config.Log.Tracef("Broadcasting from !%08x", from)

	pk, err := hex.DecodeString(strings.TrimPrefix(n.Config.NodeInfo.PKI.PublicKey, "0x"))
	if err != nil {
		n.Config.Log.Warnf("⚠️ Failed to decode public key: %v. Defaulting to empty key.\n", err)
		pk = []byte{}
	}
	n.MqttClient.PublishNodeInfo(from, ALL, whoamiTopic, n.Config.NodeInfo.LongName, n.Config.NodeInfo.ShortName, pk, meshtastic.HardwareModel(n.Config.NodeInfo.HWModelId), meshtastic.Config_DeviceConfig_CLIENT)
	n.MqttClient.PublishPosition(from, ALL, whoamiTopic, lat, lng, alt, prec)

	// Chat is opt-in. This runs on the Announce ticker (every
	// BroadcastIntervalSec, forever), so an unconditional text publish is a
	// channel-wide message repeated to every radio holding the key for as long
	// as the process lives -- exactly what the old hardcoded "hello world" did.
	if payload, ok := broadcastText(n.Config); ok {
		n.MqttClient.PublishMessageEncrypted(from, ALL, whoamiTopic, meshtastic.PortNum_TEXT_MESSAGE_APP, payload)
	}

	// NOTE: We don't want to publish the map report here, as it is not needed and unencrypted
	mapTopic := n.Config.NodeInfo.MapTopic
	longName := n.Config.NodeInfo.LongName
	shortName := n.Config.NodeInfo.ShortName
	fwVersion := n.Config.NodeInfo.Firmware
	region := n.Config.NodeInfo.Region
	modemPreset := n.Config.NodeInfo.ModemPreset
	hwModel := meshtastic.HardwareModel(n.Config.NodeInfo.HWModelId)
	n.MqttClient.PublishMapReport(from, ALL, mapTopic, longName, shortName, hwModel, meshtastic.Config_DeviceConfig_CLIENT, fwVersion, region, modemPreset, true, 4, lat, lng, alt, prec)
}

func (n *NodeInfoCmd) initNodeDb() {
	n.Nodes.LoadFile(n.Config.NodeDbPath)

	// ONE store for both the restore and the ticker: NewS3Mover builds a session
	// and validates credentials (with a paragraph of diagnostics on failure), so
	// constructing it per-caller would double that work and the log noise.
	store := n.newSnapshotStore()

	// Restore BEFORE the write loop starts. The loop below overwrites the local
	// file every 5s, so a restore that lost the race would be erased by an empty
	// database within one tick.
	n.restoreNodeSnapshot(store)

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			n.NodesMutex.Lock()
			n.restorePubKeys()
			n.Nodes.WriteFile(n.Config.NodeDbPath)
			n.NodesMutex.Unlock()
		}
	}()

	n.startNodeSnapshot(store)
}

// newSnapshotStore builds the S3-backed snapshot store, or nil when snapshots
// are not configured. Reuses network.S3Mover purely for its session and
// task-role credential handling.
func (n *NodeInfoCmd) newSnapshotStore() internal.SnapshotStore {
	bucket := n.Config.NodeSnapshotBucket
	if bucket == "" {
		return nil
	}
	region := n.Config.Server.S3BucketRegion
	if region == "" {
		region = "us-east-1"
	}
	mover, err := network.NewS3Mover(region, region, bucket)
	if err != nil {
		n.Config.Log.Errorf("node snapshot disabled: cannot init S3 for bucket %q: %v", bucket, err)
		return nil
	}
	return internal.NewS3SnapshotStore(mover.S3Client, bucket, n.Config.NodeSnapshotKey)
}

// restoreNodeSnapshot seeds a cold database from S3. A failure here is logged
// and tolerated: starting with an empty map is exactly today's behaviour, so a
// broken restore must never keep meshobserv from coming up at all.
func (n *NodeInfoCmd) restoreNodeSnapshot(store internal.SnapshotStore) {
	if store == nil {
		return
	}
	n.NodesMutex.Lock()
	defer n.NodesMutex.Unlock()

	restored, err := internal.RestoreSnapshot(&n.Nodes, store)
	if err != nil {
		n.Config.Log.Warnf("node snapshot restore failed, starting from local state: %v", err)
		return
	}
	if restored {
		n.Config.Log.Infof("node snapshot restored: %d nodes from s3://%s/%s",
			len(n.Nodes), n.Config.NodeSnapshotBucket, n.Config.NodeSnapshotKey)
	}
}

// startNodeSnapshot runs the periodic snapshot cycle. See mqtt.Snapshotter.Tick
// for why the cycle reads before it writes and why the operator reset is
// edge-triggered.
func (n *NodeInfoCmd) startNodeSnapshot(store internal.SnapshotStore) {
	if store == nil {
		return
	}
	mins := n.Config.NodeSnapshotMins
	if mins <= 0 {
		mins = 5
	}
	snapper := internal.NewSnapshotter(store)

	go func() {
		ticker := time.NewTicker(time.Duration(mins) * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			n.NodesMutex.Lock()
			n.restorePubKeys()
			res, err := snapper.Tick(&n.Nodes)
			count := len(n.Nodes)
			if res.Reset {
				// Also clear the local file immediately; otherwise the 5s write
				// loop is the only thing that syncs it and an operator watching
				// nodes.json would see the old contents linger.
				n.Nodes.WriteFile(n.Config.NodeDbPath)
			}
			n.NodesMutex.Unlock()

			switch {
			case err != nil:
				n.Config.Log.Warnf("node snapshot failed: %v", err)
			case res.Reset:
				// Loud on purpose: an operator wiped the node database. This
				// must be greppable, not inferred from a node count dropping.
				n.Config.Log.Warnf("node snapshot RESET by operator (empty {} at s3://%s/%s); node database cleared",
					n.Config.NodeSnapshotBucket, n.Config.NodeSnapshotKey)
			default:
				n.Config.Log.Debugf("node snapshot written: %d nodes", count)
			}
		}
	}()
}

func (n *NodeInfoCmd) flushNodeDb() {
	n.NodesMutex.Lock()
	n.Nodes.WriteFile(n.Config.NodeDbPath)
	n.NodesMutex.Unlock()
}
