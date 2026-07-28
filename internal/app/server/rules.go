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
			Description: "Clamp oversized hop budgets before fan-out",
			// RF flood-radius cap: every downlink-enabled radio is an MQTT→RF
			// gateway that rebroadcasts broadcasts with the packet's hop budget,
			// so one uplink with hop_limit 7 gets amplified across the whole con
			// mesh. Clamp hop_limit to 3 (the fleet/firmware default). hop_start
			// > 7 is clamped to HOP_MAX 7 — firmware rejects the whole packet at
			// ingest otherwise. hop_start is never pushed below hop_limit: 2.8
			// drops hop_start < hop_limit as provably corrupt (pre-hop drop).
			// The mutation only reaches the wire via RemarshalEnvelope — without
			// it this rule is a silent no-op (it was, until 2026-07-28).
			Matcher: func(ip *InspectorPacket) bool {
				if ip.Raw.Meshtastic == nil || ip.Raw.Meshtastic.Packet == nil {
					return false
				}
				pkt := ip.Raw.Meshtastic.Packet
				if pkt.HopLimit <= 3 && pkt.HopStart <= 7 {
					return false
				}
				if pkt.HopLimit > 3 {
					pkt.HopLimit = 3
				}
				if pkt.HopStart > 7 {
					pkt.HopStart = 7
				}
				if err := ip.RemarshalEnvelope(); err != nil {
					ip.Log.Errorf("hop clamp remarshal failed: %v", err)
					return false
				}
				return true
			},
			Action: Rewrote,
			Reason: "Hop budget clamped (RF flood-radius cap)",
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
