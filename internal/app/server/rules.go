package server

import (
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/eclipse/paho.mqtt.golang/packets"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

type Decision int

const (
	Allow Decision = iota
	Block
	Rewrote
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
		if rule.Matcher(packet) && rule.Action != Rewrote {
			return DecisionResult{
				Decision: rule.Action,
				Reason:   rule.Reason,
			}
		}
	}
	return DecisionResult{Decision: Allow, Reason: "No matching rule found"}
}

func NewRuleBasedDecider(rules []Rule) *RuleBasedDecider {
	return &RuleBasedDecider{Rules: rules}
}

func (n *ServerCmd) LoadInspectorRules() {
	n.Decider = NewRuleBasedDecider(append(n.rewriteRules(), n.inspectRules()...))
}

func (*ServerCmd) inspectRules() []Rule {
	rules := []Rule{
		{
			Name:        "AllowMQTTTypes",
			Description: "Allow MQTT connect/sub/unsub packets",
			Matcher: func(ip *InspectorPacket) bool {
				switch (*ip.Raw.MQTT).(type) {
				case *packets.ConnectPacket,
					*packets.SubscribePacket,
					*packets.PubackPacket,
					*packets.PingreqPacket,
					*packets.UnsubscribePacket,
					*packets.PublishPacket:
					return true
				default:
					return false
				}
			},
			Action: Allow,
			Reason: "MQTT Connect packets are allowed",
		},
		{
			Name:        "BlockInvalidEncryption",
			Description: "Block packets that failed to decrypt with any known key",
			Matcher: func(packet *InspectorPacket) bool {
				return (!packet.Meshtastic.WasPKIEncrypted) &&
					(packet.Meshtastic.WasEncrypted && !packet.Meshtastic.WasUnmarshalled)
			},
			Action: Block,
			Reason: "Failed to decrypt with any known key",
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
	}
	return rules
}

func (n *ServerCmd) rewriteRules() []Rule {
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

				n.RewritePayloadString(ip)

				return true
			},
			Action: Rewrote,
			Reason: "MQTT Connect packets are rewritten",
		},
	}

}

func (n *ServerCmd) RewritePayloadString(ip *InspectorPacket) (error, bool) {
	if ip.Meshtastic.WasPKIEncrypted {
		return fmt.Errorf("cannot rewrite packet is PKI encrypted"), false
	}

	dataBytes, _ := proto.Marshal(&meshtastic.Data{
		Portnum: ip.Meshtastic.PortNum,
		Payload: []byte(ip.Meshtastic.PayloadString),
	})

	// Prepare the encrypted payload
	encrypted := make([]byte, len(dataBytes))
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], ip.Meshtastic.Id)
	binary.LittleEndian.PutUint32(nonce[8:], ip.Meshtastic.From)
	base64Key := ip.Meshtastic.HexKey
	keyBytes, _ := base64.StdEncoding.DecodeString(base64Key)
	if len(keyBytes) == 1 {
		keyBytes = append(keyBytes, make([]byte, 15)...)
	}
	c := NewAESCipher(keyBytes)
	cipher.NewCTR(c, nonce).XORKeyStream(encrypted, dataBytes)

	ip.Raw.Meshtastic.Packet.PayloadVariant = &meshtastic.MeshPacket_Encrypted{
		Encrypted: encrypted,
	}

	switch p := (*ip.Raw.MQTT).(type) {
	case *packets.PublishPacket:
		payloadBytes, err := proto.Marshal(ip.Raw.Meshtastic)
		if err != nil {
			return fmt.Errorf("failed to marshal Meshtastic payload: %v", err), false
		}
		p.Payload = payloadBytes
	}

	return nil, false
}
