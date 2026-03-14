package server

import (
	"strings"

	"github.com/eclipse/paho.mqtt.golang/packets"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)

func (n *ServerCmd) LoadInspectorRules() {
	n.PacketDecider = NewRuleBasedDecider(append(rewriteRules(), inspectRules()...))
}

func inspectRules() []Rule {
	rules := []Rule{
		{
			Name:        "AllowMQTTControl",
			Description: "Allow MQTT control packets (not PUBLISH — those go through meshtastic inspection)",
			Matcher: func(ip *InspectorPacket) bool {
				switch (*ip.Raw.MQTT).(type) {
				case *packets.ConnectPacket,
					*packets.SubscribePacket,
					*packets.PubackPacket,
					*packets.PingreqPacket,
					*packets.UnsubscribePacket,
					*packets.DisconnectPacket:
					return true
				default:
					return false
				}
			},
			Action: Allow,
			Reason: "MQTT control packets are allowed",
		},
		{
			Name:        "AllowPKIEncrypted",
			Description: "Allow PKI-encrypted packets (point-to-point, can't validate PSK)",
			Matcher: func(packet *InspectorPacket) bool {
				return packet.Meshtastic.WasPKIEncrypted
			},
			Action: Allow,
			Reason: "PKI-encrypted packets are allowed",
		},
		{
			Name:        "BlockInvalidEncryption",
			Description: "Block packets that had encrypted data but failed to decrypt with any known channel key",
			Matcher: func(packet *InspectorPacket) bool {
				return packet.Meshtastic.HadEncryptedPayload && !packet.Meshtastic.WasEncrypted
			},
			Action: Block,
			Reason: "Failed to decrypt with any known key",
		},
		{
			Name:        "RequireMQTTUserName",
			Description: "Require a MQTT username",
			Matcher: func(packet *InspectorPacket) bool {
				return packet.Track.Username == ""
			},
			Action: Block,
			Reason: "Username required for MQTT",
		},
		{
			Name:        "AllowedMeshtasticApps",
			Description: "Always allow NodeInfo packets",
			Matcher: func(packet *InspectorPacket) bool {
				return packet.Meshtastic.PortNum == meshtastic.PortNum_NODEINFO_APP ||
					packet.Meshtastic.PortNum == meshtastic.PortNum_POSITION_APP ||
					packet.Meshtastic.PortNum == meshtastic.PortNum_TEXT_MESSAGE_APP
			},
			Action: Allow,
			Reason: "NodeInfo/Position/Text Message packets are always allowed",
		},
		{
			Name:        "AllowTelemetryForSensorUser",
			Description: "Allow telemetry packets for sensor users",
			Matcher: func(packet *InspectorPacket) bool {
				return packet.Meshtastic.PortNum == meshtastic.PortNum_TELEMETRY_APP &&
					packet.Track.Username == "sensor"
			},
			Action: Allow,
			Reason: "Telemetry packets are allowed for 'public' users",
		},
	}
	return rules
}

func rewriteRules() []Rule {
	return []Rule{
		{
			Name:        "RewriteHopLimit",
			Description: "Rewrite packets to adjust hop limit",
			Matcher: func(ip *InspectorPacket) bool {
				// Check if the packet is a Meshtastic packet
				if ip.Raw.Meshtastic == nil ||
					ip.Raw.Meshtastic.Packet == nil ||
					ip.Raw.Meshtastic.Packet.HopLimit <= 3 {
					return false
				}
				ip.Raw.Meshtastic.Packet.HopLimit = 3
				return true
			},
			Action: Rewrote,
			Reason: "MQTT Connect packets are rewritten",
		},
		{
			Name:        "RewriteHelloGoodbye",
			Description: "Replace words in channel messages",
			Matcher: func(ip *InspectorPacket) bool {
				// Check if the packet is a Meshtastic packet that's not PKI
				if ip.Raw.Meshtastic == nil ||
					ip.Raw.Meshtastic.Packet == nil ||
					ip.Meshtastic.Decoded == nil ||
					ip.Meshtastic.Decoded.Portnum != meshtastic.PortNum_TEXT_MESSAGE_APP ||
					ip.Meshtastic.WasPKIEncrypted {
					return false
				}

				if ip.Track.Username == "public" {
					ip.Meshtastic.PayloadString = strings.ReplaceAll(ip.Meshtastic.PayloadString, "hi", "bye")
					ip.Meshtastic.PayloadString = strings.ReplaceAll(ip.Meshtastic.PayloadString, "hello", "👋")
					ip.Meshtastic.PayloadString = strings.ReplaceAll(ip.Meshtastic.PayloadString, "fuck", "🤬")
				}

				ip.RewritePayloadString()

				return true
			},
			Action: Rewrote,
			Reason: "MQTT Connect packets are rewritten",
		},
	}

}
