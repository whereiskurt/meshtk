package nodeinfo

import (
	"fmt"
	"os"
	"sync"
	"time"

	mqtt "github.com/whereiskurt/meshtk/internal/mqtt"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	proto "google.golang.org/protobuf/proto"
)

type MessageLedger struct {
	Sequence      uint32             `json:"sequence"`
	DateTimeStamp int64              `json:"dts"`
	To            uint32             `json:"to"`
	From          uint32             `json:"from"`
	ToNode        mqtt.Node          `json:"toNode"`
	FromNode      mqtt.Node          `json:"fromNode"`
	Topic         string             `json:"topic"`
	PortNum       meshtastic.PortNum `json:"portNum"`
	Payload       []byte             `json:"payload"`
}

var sequence uint32

func (n *NodeInfoCmd) AddMessageLedger(to, from uint32, topic string, portNum meshtastic.PortNum, payload []byte) {
	MessagesMutex.Lock()
	defer MessagesMutex.Unlock()

	fromNode := n.Nodes[from]
	toNode := n.Nodes[to]

	if fromNode == nil {
		fromNode = mqtt.NewNode(topic)
	}
	if toNode == nil {
		toNode = mqtt.NewNode(topic)
		toNode.ShortName = "ALL"
	}
	message := MessageLedger{
		Sequence:      sequence,
		DateTimeStamp: time.Now().Unix(),
		To:            to,
		ToNode:        *toNode,
		FromNode:      *fromNode,
		From:          from,
		Topic:         topic,
		PortNum:       portNum,
		Payload:       payload,
	}

	Messages = append(Messages, message)

	file, err := os.OpenFile("message_ledger.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		defer file.Close()
		logMessage := fmt.Sprintf("%d,%v,[%s:!%08x]->%v->[%s:!%08x]:%s\n", sequence, message.DateTimeStamp, fromNode.ShortName, from, message.PortNum, toNode.ShortName, to, message.Topic)
		file.WriteString(logMessage)
	} else {
		fmt.Printf("Failed to write to log file: %v\n", err)
	}
	sequence++
}

var Messages []MessageLedger
var MessagesMutex sync.Mutex

// NodeHandler is the callback function for handling incoming messages from the MQTT client.
func (n *NodeInfoCmd) NodeHandler(to, from uint32, topic string, portNum meshtastic.PortNum, payload []byte) {
	switch portNum {
	case meshtastic.PortNum_TEXT_MESSAGE_APP:
		n.Config.Log.Tracef(`{from: '%v', topic: '%v', message: '%s'}`, from, topic, payload)
		n.AddMessageLedger(to, from, topic, portNum, payload)

	case meshtastic.PortNum_POSITION_APP:
		var position meshtastic.Position
		if err := proto.Unmarshal(payload, &position); err != nil {
			n.Config.Log.Warnf(`{error: '%v', from: '%v', topic: '%v'}`, err, from, topic)
			return
		}

		latitude := position.GetLatitudeI()
		longitude := position.GetLongitudeI()
		altitude := position.GetAltitude()
		precision := position.GetPrecisionBits()
		n.Config.Log.Tracef(`{'from': '%v', 'topic': '%v', 'portNum': '%s', 'latitude': %v, 'longitude': %v, 'altitude': %v, 'precision': %v/32}`, from, topic, portNum, latitude, longitude, altitude, precision)
		if latitude == 0 && longitude == 0 {
			return
		}
		n.NodesMutex.Lock()
		if n.Nodes[from] == nil {
			n.Nodes[from] = mqtt.NewNode(topic)
		}
		n.Nodes[from].UpdatePosition(latitude, longitude, altitude, precision)
		n.Nodes[from].UpdateSeenBy(topic)
		n.AddMessageLedger(to, from, topic, portNum, payload)
		n.NodesMutex.Unlock()
	case meshtastic.PortNum_NODEINFO_APP:
		var user meshtastic.User
		if err := proto.Unmarshal(payload, &user); err != nil {
			n.Config.Log.Warnf(`{error: '%v', from: '%v', topic: '%v'}`, err, from, topic)
			return
		}

		id := user.GetId()
		longName := user.GetLongName()
		shortName := user.GetShortName()
		hwModel := user.GetHwModel().String()
		role := user.GetRole().String()
		pubkey := user.GetPublicKey()

		n.Config.Log.Tracef(`{'from': '%v', 'to': '%v', 'topic': '%v', 'portNum': '%s', 'longName': '%v', 'shortName': '%v', 'hwModel': '%v', 'role': '%v', 'pubkey': '%v'}`, from, id, topic, portNum, longName, shortName, hwModel, role, pubkey)
		if len(longName) == 0 {
			return
		}
		n.NodesMutex.Lock()
		if n.Nodes[from] == nil {
			n.Nodes[from] = mqtt.NewNode(topic)
		}
		n.Nodes[from].UpdateUser(from, longName, shortName, hwModel, role, pubkey)
		n.AddMessageLedger(to, from, topic, portNum, payload)
		n.NodesMutex.Unlock()
	case meshtastic.PortNum_TELEMETRY_APP:
		var telemetry meshtastic.Telemetry
		if err := proto.Unmarshal(payload, &telemetry); err != nil {
			n.Config.Log.Warnf(`{error: '%v', from: '%v', topic: '%v'}`, err, from, topic)
			return
		}

		if deviceMetrics := telemetry.GetDeviceMetrics(); deviceMetrics != nil {
			batteryLevel := deviceMetrics.GetBatteryLevel()
			voltage := deviceMetrics.GetVoltage()
			chUtil := deviceMetrics.GetChannelUtilization()
			airUtilTx := deviceMetrics.GetAirUtilTx()
			uptime := deviceMetrics.GetUptimeSeconds()
			n.Config.Log.Tracef(
				`{'from': '%v', 'topic': '%v', 'portNum': '%s', 'DeviceMetrics': {'batteryLevel': %v, 'voltage': %v, 'channelUtilization': %v, 'airUtilTx': %v, 'uptime': %v}}`,
				from, topic, portNum.String(), batteryLevel, voltage, chUtil, airUtilTx, uptime,
			)
			n.NodesMutex.Lock()
			if n.Nodes[from] == nil {
				n.Nodes[from] = mqtt.NewNode(topic)
			}
			n.Nodes[from].UpdateDeviceMetrics(batteryLevel, voltage, chUtil, airUtilTx, uptime)
			n.AddMessageLedger(to, from, topic, portNum, payload)
			n.NodesMutex.Unlock()
		} else if envMetrics := telemetry.GetEnvironmentMetrics(); envMetrics != nil {
			temperature := envMetrics.GetTemperature()
			relativeHumidity := envMetrics.GetRelativeHumidity()
			barometricPressure := envMetrics.GetBarometricPressure()
			lux := envMetrics.GetLux()
			windDirection := envMetrics.GetWindDirection()
			windSpeed := envMetrics.GetWindSpeed()
			windGust := envMetrics.GetWindGust()
			radiation := envMetrics.GetRadiation()
			rainfall1 := envMetrics.GetRainfall_1H()
			rainfall24 := envMetrics.GetRainfall_24H()
			n.Config.Log.Tracef(
				`{'from': '%v', 'topic': '%v', 'portNum': '%s', 'EnvironmentMetrics': {'temperature': %v, 'relativeHumidity': %v, 'barometricPressure': %v, 'lux': %v, 'windDirection': %v, 'windSpeed': %v, 'windGust': %v, 'radiation': %v, 'rainfall1H': %v, 'rainfall24H': %v}}`,
				from, topic, portNum, temperature, relativeHumidity, barometricPressure, lux,
				windDirection, windSpeed, windGust, radiation, rainfall1, rainfall24,
			)
			n.NodesMutex.Lock()
			if n.Nodes[from] == nil {
				n.Nodes[from] = mqtt.NewNode(topic)
			}
			n.Nodes[from].UpdateEnvironmentMetrics(
				temperature,
				relativeHumidity,
				barometricPressure,
				lux,
				windDirection,
				windSpeed,
				windGust,
				radiation,
				rainfall1,
				rainfall24,
			)
			n.AddMessageLedger(to, from, topic, portNum, payload)
			n.NodesMutex.Unlock()
		}
	case meshtastic.PortNum_NEIGHBORINFO_APP:
		var neighborInfo meshtastic.NeighborInfo
		if err := proto.Unmarshal(payload, &neighborInfo); err != nil {
			n.Config.Log.Warnf(`{error: '%v', from: '%v', topic: '%v'}`, err, from, topic)
			return
		}

		nodeNum := neighborInfo.GetNodeId()
		neighbors := neighborInfo.GetNeighbors()
		n.Config.Log.Tracef(`{from: '%v', topic: '%v', portNum: '%s', nodeNum: '%v', neighbors: '%v'}`, from, topic, portNum, nodeNum, len(neighbors))
		if nodeNum != from {
			return
		}
		if len(neighbors) == 0 {
			return
		}
		n.NodesMutex.Lock()
		if n.Nodes[from] == nil {
			n.Nodes[from] = mqtt.NewNode(topic)
		}
		for _, neighbor := range neighbors {
			neighborNum := neighbor.GetNodeId()
			if neighborNum == 0 {
				continue
			}
			n.Nodes[from].UpdateNeighborInfo(neighborNum, neighbor.GetSnr())
		}
		n.AddMessageLedger(to, from, topic, portNum, payload)
		n.NodesMutex.Unlock()
	case meshtastic.PortNum_MAP_REPORT_APP:
		var mapReport meshtastic.MapReport
		if err := proto.Unmarshal(payload, &mapReport); err != nil {
			n.Config.Log.Warnf(`{error: '%v', from: '%v', topic: '%v'}`, err, from, topic)
			return
		}
		n.Config.Log.Tracef(`{mapReport: '%s'}`, &mapReport)

		longName := mapReport.GetLongName()
		shortName := mapReport.GetShortName()
		hwModel := mapReport.GetHwModel().String()
		role := mapReport.GetRole().String()
		fwVersion := mapReport.GetFirmwareVersion()
		region := mapReport.GetRegion().String()
		modemPreset := mapReport.GetModemPreset().String()
		hasDefaultCh := mapReport.GetHasDefaultChannel()
		onlineLocalNodes := mapReport.GetNumOnlineLocalNodes()
		latitude := mapReport.GetLatitudeI()
		longitude := mapReport.GetLongitudeI()
		altitude := mapReport.GetAltitude()
		precision := mapReport.GetPositionPrecision()

		n.Config.Log.Tracef(
			`{'from': '%v', 'topic': '%v', 'portNum': '%s', 'longName': '%v', 'shortName': '%v', 'hwModel': '%v', 'role': '%v', 'fwVersion': '%v', 'region': '%v', 'modemPreset': '%v', 'hasDefaultCh': %v, 'onlineLocalNodes': %v, 'latitude': %v, 'longitude': %v, 'altitude': %v, 'precision': %v}`,
			from, topic, portNum,
			longName, shortName, hwModel, role, fwVersion, region, modemPreset, hasDefaultCh, onlineLocalNodes,
			latitude, longitude, altitude, precision,
		)

		if len(longName) == 0 {
			return
		}
		if latitude == 0 && longitude == 0 {
			return
		}
		n.NodesMutex.Lock()
		if n.Nodes[from] == nil {
			n.Nodes[from] = mqtt.NewNode(topic)
		}
		n.Nodes[from].UpdateUser(from, longName, shortName, hwModel, role, nil)
		n.Nodes[from].UpdateMapReport(fwVersion, region, modemPreset, hasDefaultCh, onlineLocalNodes)
		n.Nodes[from].UpdatePosition(latitude, longitude, altitude, precision)
		n.Nodes[from].UpdateSeenBy(topic)
		n.AddMessageLedger(to, from, topic, portNum, payload)
		n.NodesMutex.Unlock()
	default:
		n.AddMessageLedger(to, from, topic, portNum, payload)
		n.Config.Log.Tracef(`{from: '%v', topic: '%v', portNum: '%s'}`, from, topic, portNum)
	}
}
