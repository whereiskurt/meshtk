package server

import (
	"strings"

	v5 "github.com/eclipse/paho.golang/packets"
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
				// The 3.1.1 branch is reached FIRST and is unedited, so the
				// decision sequence proxy_v4_golden_test.go pins cannot move.
				// A v5 packet carries Raw.MQTT5 and leaves Raw.MQTT nil, and a
				// bare dereference would panic on it -- which takes down the
				// whole proxy process, not one connection.
				if ip.Raw.MQTT != nil {
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
				}
				// Same allowlist on the v5 codec, so the control-packet rule
				// gives identical answers to both. A codec-dependent answer here
				// is a codec-dependent answer for every rule below it, since
				// this is the first inspect rule and it short-circuits.
				if ip.Raw.MQTT5 != nil {
					switch ip.Raw.MQTT5.Content.(type) {
					case *v5.Connect,
						*v5.Subscribe,
						*v5.Puback,
						*v5.Pingreq,
						*v5.Unsubscribe,
						*v5.Disconnect:
						return true
					default:
						return false
					}
				}
				// Third branch, reached ONLY for a v5 SUBSCRIBE the codec
				// refused to parse (proxy_v5_rawsubscribe.go). Before it, such
				// a frame never reached the decider at all: it was relayed
				// without an InspectorPacket, so MQTT.Topics was empty and the
				// first topic Block rule anyone added silently would not apply
				// to it -- the same client-chosen property bytes as CR-04
				// buying an exemption one layer up (68-REVIEW WR-04).
				//
				// A SUBSCRIBE is a control packet, so the answer is the same
				// `true` the other two branches give it. Answering differently
				// here would make the FIRST inspect rule codec-dependent, and
				// since it short-circuits, every rule below it too.
				//
				// The rejected alternative was synthesizing a v5.Subscribe so
				// the branch above would match unchanged. That makes Raw.MQTT5
				// lie about where the data came from, and RawPacket's
				// never-synthesize invariant exists because a synthesized
				// packet is one a rule can mutate without the mutation ever
				// reaching the wire (meshtk#22).
				if ip.Raw.MQTT5RawSub != nil {
					return true
				}
				// No codec populated: nothing to allow.
				return false
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
			// Declining is the only honest outcome for a packet that arrived
			// UNENCRYPTED: the censor's contract is re-encrypt-then-forward, and a
			// decoded packet has no channel cipher to re-encrypt with, so there is
			// nothing here to rewrite. The old matcher entered anyway and
			// RewritePayloadString dereferenced the nil cipher -- a SIGSEGV in the
			// read loop with no recover() above it, so one authenticated plaintext
			// text message killed the whole process (review CR-01).
			//
			// Worth recording WHY that was fleet-wide rather than rare: the word
			// replacement below is gated on the "public" username, but the rewrite
			// CALL never was. Every text message from every user therefore
			// traversed both the crash site (CR-01) and the field-dropping Data
			// rebuild (CR-03), not just the censored ones.
			Matcher: func(ip *InspectorPacket) bool {
				// Check if the packet is a Meshtastic packet that's not PKI, and
				// that we can actually re-encrypt what we are about to change.
				if ip.Raw.Meshtastic == nil ||
					ip.Raw.Meshtastic.Packet == nil ||
					ip.Meshtastic.Decoded == nil ||
					ip.Meshtastic.Decoded.Portnum != meshtastic.PortNum_TEXT_MESSAGE_APP ||
					ip.Meshtastic.WasPKIEncrypted ||
					!ip.Meshtastic.WasEncrypted ||
					ip.Meshtastic.Cipher == nil {
					return false
				}

				if ip.Track.Username == "public" {
					ip.Meshtastic.PayloadString = strings.ReplaceAll(ip.Meshtastic.PayloadString, "hi", "bye")
					ip.Meshtastic.PayloadString = strings.ReplaceAll(ip.Meshtastic.PayloadString, "hello", "👋")
					ip.Meshtastic.PayloadString = strings.ReplaceAll(ip.Meshtastic.PayloadString, "fuck", "🤬")
				}

				// Consume the error, exactly as RewriteHopLimit does above.
				// Returning true after a failed rewrite is the meshtk#22 silent
				// no-op class: the rule reports Rewrote while the ORIGINAL bytes
				// forward to the broker.
				if err := ip.RewritePayloadString(); err != nil {
					ip.Log.Errorf("payload censor failed: %v", err)
					return false
				}

				return true
			},
			Action: Rewrote,
			Reason: "MQTT Connect packets are rewritten",
		},
	}

}
