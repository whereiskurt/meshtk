package protoserver

import (
	"strings"

	"github.com/eclipse/paho.mqtt.golang/packets"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)

type Decision int

const (
	Keep Decision = iota
	Drop
	Block
	Rewritten
)

type DecisionResult struct {
	Decision Decision
	Reason   string
}

type Decider interface {
	Decide(packet *InspectorPacket) DecisionResult
}

type RuleBasedDecider struct {
	Rules []Rule
}

type Rule struct {
	Name        string
	Description string
	Matcher     func(packet *InspectorPacket) bool
	Action      Decision
	Reason      string
}

// Decide implements the Decider interface
func (d *RuleBasedDecider) Decide(packet *InspectorPacket) DecisionResult {
	for _, rule := range d.Rules {
		if rule.Matcher(packet) && rule.Action != Rewritten {
			return DecisionResult{
				Decision: rule.Action,
				Reason:   rule.Reason,
			}
		}
	}
	return DecisionResult{Decision: Keep, Reason: "No matching rule found"}
}

func NewRuleBasedDecider(rules []Rule) *RuleBasedDecider {
	return &RuleBasedDecider{Rules: rules}
}

func (n *ProtoBufServerCmd) LoadInspectorRules() {
	rules := []Rule{
		{
			Name:        "AllowMQTTTypes",
			Description: "Allow MQTT connect/sub/unsub packets",
			Matcher: func(ip *InspectorPacket) bool {
				switch (*ip.Raw.MQTT).(type) {
				case *packets.ConnectPacket, *packets.SubscribePacket, *packets.UnsubscribePacket, *packets.PublishPacket:
					return true
				default:
					return false
				}
			},
			Action: Keep,
			Reason: "MQTT Connect packets are allowed",
		},
		{
			Name:        "DropMQTTTypes",
			Description: "Block MQTT ping packets",
			Matcher: func(ip *InspectorPacket) bool {
				switch (*ip.Raw.MQTT).(type) {
				case *packets.PingreqPacket, *packets.PingrespPacket:
					return true
				default:
					return false
				}
			},
			Action: Block,
			Reason: "MQTT type blocked",
		},
		{
			Name:        "BlockInvalidEncryption",
			Description: "Block packets that failed to decrypt with any known key",
			Matcher: func(packet *InspectorPacket) bool {
				//TODO: Double check this works with PKI messages we didn't decrypt
				return packet.Meshtastic.WasEncrypted && !packet.Meshtastic.WasUnmarshalled
			},
			Action: Block,
			Reason: "Failed to decrypt with any known key",
		},
		{
			Name:        "AllowNodeInfo",
			Description: "Always allow NodeInfo packets",
			Matcher: func(packet *InspectorPacket) bool {
				return packet.Meshtastic.PortNum == meshtastic.PortNum_NODEINFO_APP
			},
			Action: Keep,
			Reason: "NodeInfo packets are always allowed",
		},
		{
			Name:        "AllowPositionInfo",
			Description: "Always allow Position packets",
			Matcher: func(packet *InspectorPacket) bool {
				return packet.Meshtastic.PortNum == meshtastic.PortNum_POSITION_APP
			},
			Action: Keep,
			Reason: "Position packets are always allowed",
		},
		{
			Name:        "FilterByClientID",
			Description: "Block packets from clients with suspicious IDs",
			Matcher: func(packet *InspectorPacket) bool {
				// Block packets from clients with IDs containing "malicious"
				return packet.Track != nil &&
					packet.Track.ClientID != "" &&
					strings.Contains(strings.ToLower(packet.Track.ClientID), "malicious")
			},
			Action: Block,
			Reason: "Suspicious client ID detected",
		},
		{
			Name:        "FilterByUsername",
			Description: "Block packets from users with suspicious usernames",
			Matcher: func(packet *InspectorPacket) bool {
				// Block packets from specific usernames
				return packet.Track != nil &&
					packet.Track.Username != "" &&
					strings.Contains(strings.ToLower(packet.Track.Username), "banned")
			},
			Action: Block,
			Reason: "Banned username detected",
		},
	}

	n.Decider = NewRuleBasedDecider(append(n.rewriteRules(), rules...))
}

func (n *ProtoBufServerCmd) rewriteRules() []Rule {
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

				n.Config.Log.Tracef("Rewriting hop limit for packet: %+v", ip.Raw.Meshtastic)
				ip.Raw.Meshtastic.Packet.HopLimit = 3

				return true
			},
			Action: Rewritten,
			Reason: "MQTT Connect packets are rewritten",
		},
	}

}
