package nodeinfo

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"

	"github.com/spf13/cobra"
	"github.com/whereiskurt/meshtk/internal/app/help"
	internal "github.com/whereiskurt/meshtk/internal/mqtt"
	"github.com/whereiskurt/meshtk/pkg/config"
)

type NodeInfoCmd struct {
	Nodes      internal.NodeDB
	NodesMutex sync.Mutex
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

	return n
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

	n.MqttClient = internal.NewMqttClient(n.Config, &n.Nodes)

	n.MqttClient.SetMessageHandler(n.NodeHandler)
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
	fromUint, err := strconv.ParseUint(fromMeshHex[1:], 16, 32) // We start at 1 to skip the '!'
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
	n.MqttClient.PublishNodeInfo(from, ALL, whoamiTopic, n.Config.NodeInfo.LongName, n.Config.NodeInfo.ShortName, meshtastic.HardwareModel(n.Config.NodeInfo.HWModelId), meshtastic.Config_DeviceConfig_CLIENT)
	n.MqttClient.PublishPosition(from, ALL, whoamiTopic, lat, lng, alt, prec)

	n.MqttClient.PublishMessageEncrypted(from, ALL, whoamiTopic, meshtastic.PortNum_TEXT_MESSAGE_APP, []byte{'h', 'e', 'l', 'l', 'o', ' ', 'w', 'o', 'r', 'l', 'd'})

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
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			n.NodesMutex.Lock()
			n.Nodes.WriteFile(n.Config.NodeDbPath)
			n.NodesMutex.Unlock()
		}
	}()
}

func (n *NodeInfoCmd) flushNodeDb() {
	n.NodesMutex.Lock()
	n.Nodes.WriteFile(n.Config.NodeDbPath)
	n.NodesMutex.Unlock()
}
