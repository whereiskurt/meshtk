package server

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strings"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	"github.com/whereiskurt/meshtk/pkg/network"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

var rateLimiter = network.NewLimiter(
	1,              // tokens per second
	3,              // allow a burst size / start size
	20*time.Minute, // inactivity window
	5,              // threshold tokens for kill
)

const socketPenalty = time.Duration(2000) * time.Millisecond

// An idle MQTT client is SUPPOSED to be silent between keepalives, so a short
// read deadline on the client socket is a connection killer, not a safety net.
// A hardcoded 10s here tore down every radio that had nothing to publish for
// 10 seconds -- measured 42 CONNECTs in 30 minutes from one BLE-proxied radio,
// which read as "the device is flapping" when in fact the proxy was hanging up
// on it. Chat traffic masked it: during an active exchange both sides transmit
// constantly and the window never expires.
//
// MQTT-3.1.1 §3.1.2.10: the server SHOULD disconnect only after 1.5x keepalive
// without a packet. proxyReadTimeout implements that, with a floor so a tiny
// negotiated keepalive cannot reintroduce the same teardown loop, and a default
// for clients whose CONNECT we never saw.
const (
	defaultProxyReadTimeout = 180 * time.Second
	minProxyReadTimeout     = 60 * time.Second
)

// proxyReadTimeout converts a CONNECT keepalive (seconds) into how long the
// proxy waits for the next packet before giving up on the client.
func proxyReadTimeout(keepaliveSecs uint16) time.Duration {
	if keepaliveSecs == 0 {
		return defaultProxyReadTimeout
	}
	d := time.Duration(keepaliveSecs) * time.Second * 3 / 2
	if d < minProxyReadTimeout {
		return minProxyReadTimeout
	}
	return d
}

// peekConnectProtocolVersion peeks at the first client packet without
// consuming it and, when it is a CONNECT with protocol name "MQTT", returns
// the protocol-level byte (4 = 3.1.1, 5 = v5). ok is false for anything it
// cannot positively identify — non-CONNECT first packets, the legacy
// "MQIsdp" 3.1 name, or short/garbage streams — which all fall through to
// the normal 3.1.1 codec path unchanged.
//
// Peek (not Read) matters: a 3.1.1 CONNECT must still be fully readable by
// packets.ReadPacket afterwards.
func peekConnectProtocolVersion(r *bufio.Reader) (byte, bool) {
	// 1 type byte + up to 4 remaining-length varint bytes + 2-byte name
	// length + "MQTT" + version byte = 12 bytes max.
	hdr, err := r.Peek(12)
	if err != nil || len(hdr) < 12 {
		return 0, false
	}
	if hdr[0]&0xF0 != 0x10 { // not CONNECT
		return 0, false
	}
	i := 1 // skip the remaining-length varint (1-4 bytes)
	for ; i <= 4; i++ {
		if hdr[i]&0x80 == 0 {
			break
		}
	}
	i++ // first byte of the variable header
	if i+7 > len(hdr) {
		return 0, false
	}
	if hdr[i] != 0x00 || hdr[i+1] != 0x04 || string(hdr[i+2:i+6]) != "MQTT" {
		return 0, false
	}
	return hdr[i+6], true
}

// writeMqtt5UnsupportedConnack answers a v5 client in its own dialect:
// CONNACK with ack-flags 0x00, reason code 0x84 (Unsupported Protocol
// Version), empty properties. The 3.1.1 codepath's 0x05 rejection is
// meaningless in v5 and renders in the Meshtastic app as a bogus
// "check credentials" error.
func writeMqtt5UnsupportedConnack(conn net.Conn) error {
	_, err := conn.Write([]byte{0x20, 0x03, 0x00, 0x84, 0x00})
	return err
}

func (n *ServerCmd) handleProxy(conn net.Conn) {
	socketAddr := n.TrackConnection(conn)

	defer func(conn net.Conn, socketAddr string) {
		n.ConnMutex.Lock()
		delete(n.ConnTrack, socketAddr)
		n.ConnMutex.Unlock()
		conn.Close()
	}(conn, socketAddr)

	request := bufio.NewReader(conn)

	// MQTT v5 preflight: the paho 3.1.1 codec used below never checks the
	// version byte, so a v5 CONNECT's properties block bleeds into the
	// credential fields and valid creds get rejected as "check credentials"
	// (Meshtastic-Android 2.8.0's mqttastic client speaks v5 — upstream
	// Meshtastic-Android#6505). Levels ABOVE 5 the proxy cannot speak: reject
	// honestly with a version-correct CONNACK before dialing the backend.
	// Level 5 itself now has a real codec and dispatches into its own handler
	// (proxy_v5.go) -- one branch, so the 3.1.1 body below is untouched.
	conn.SetReadDeadline(time.Now().Add(defaultProxyReadTimeout))
	if ver, ok := peekConnectProtocolVersion(request); ok && ver > 5 {
		n.InspectorLogger.Warnf("action=MQTT5_REJECT, ip=%s, protocol_version=%d, reason=unsupported_protocol_version",
			socketAddr, ver)
		writeMqtt5UnsupportedConnack(conn)
		return
	} else if ok && ver == 5 {
		n.handleProxyV5(conn, request, socketAddr)
		return
	}

	backendConn, err := net.DialTimeout("tcp", n.Config.Server.ProxyForwardAddress, 10*time.Second)
	if err != nil {
		n.Config.Log.Errorf("failed to connect to backend: %v", err)
		return
	}
	defer backendConn.Close()
	backendReader := bufio.NewReader(backendConn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure we cancel the context when this function exits

	go n.handleBackend(ctx, conn, socketAddr, backendConn, backendReader)

	// Starts permissive; tightened to 1.5x once the client's CONNECT tells us
	// its keepalive. Only this goroutine touches the client read deadline --
	// handleBackend used to clobber it from the downlink side.
	readTimeout := defaultProxyReadTimeout

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// Update the read deadline for each packet
			conn.SetReadDeadline(time.Now().Add(readTimeout))

			packet, err := packets.ReadPacket(request)
			if err != nil {
				backendConn.Close()
				return
			}

			if cp, ok := packet.(*packets.ConnectPacket); ok {
				readTimeout = proxyReadTimeout(cp.Keepalive)
			}

			ip := &InspectorPacket{
				Log:   n.InspectorLogger,
				Track: &ConnectionInfo{SocketAddress: socketAddr},
				Raw:   &RawPacket{MQTT: &packet},
			}

			ip.inspectRawPacket(n, conn)

			if ip.AuthRejected {
				return
			}

			// TODO: Build this out as an actual ALLOW_LIST
			shouldInspect := true
			if strings.Contains(strings.ToLower(ip.Track.ClientID), "kphkphkph") {
				shouldInspect = false
			}

			if shouldInspect {
				// Apply decision rules
				result := n.PacketDecider.Decide(ip)

				// RATE LIMITING DISABLED 2026-07-19 (con debug): the SLOW/KILL
				// enforcement was penalizing/tearing down real BLE-proxied radios on
				// reconnect bursts, disrupting downlink. Allow-all pass-through; no
				// throttle. Re-enable post-con by restoring EnforceLimit below.
				// slowed, kill, tokenCount := rateLimiter.EnforceLimit(socketAddr, socketPenalty)
				// penalty := min(socketPenalty*time.Duration(-tokenCount+1), 10*time.Second)
				// if slowed {
				// 	ip.WriteLimiterLog(Slow, tokenCount, penalty)
				// } else if kill {
				// 	ip.WriteLimiterLog(Kill, tokenCount, penalty)
				// 	return
				// }

				switch result.Decision {
				case Allow:
					if n.Config.Server.ShouldLogAllows {
						ip.WriteDecisionLog(result)
					}
					if ip.Meshtastic.WasUnmarshalled {
						n.Config.Log.Infof("[proxy] ALLOW from=!%08x to=!%08x type=%s topic=%v user=%s",
							ip.Meshtastic.From, ip.Meshtastic.To, ip.Meshtastic.PortNum.String(), ip.MQTT.Topics, ip.Track.Username)
					}
				case Block:
					if n.Config.Server.ShouldLogBlocks {
						ip.WriteDecisionLog(result)
					}
					n.Config.Log.Warnf("[proxy] BLOCK from=!%08x to=!%08x reason=%q user=%s ip=%s",
						ip.Meshtastic.From, ip.Meshtastic.To, result.Reason, ip.Track.Username, ip.Track.SocketAddress)
					return
				default:
					if n.Config.Server.ShouldLogAllows || n.Config.Server.ShouldLogBlocks {
						ip.WriteDecisionLog(result)
					}
				}
			}

			// Serialize the packet for forwarding
			var buf bytes.Buffer
			if err := (*ip.Raw.MQTT).Write(&buf); err != nil {
				n.Config.Log.Errorf("failed to serialize MQTT packet: %v", err)
				return
			}

			// Forward the packet to the backend
			if _, err := backendConn.Write(buf.Bytes()); err != nil {
				n.Config.Log.Errorf("failed to write to backend: %v", err)
				return
			}
		}
	}
}

func (n *ServerCmd) handleBackend(ctx context.Context, conn net.Conn, socketAddr string, backendConn net.Conn, backendReader *bufio.Reader) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			// Deadline belongs on the socket we are READING (the backend). This
			// previously deadlined `conn`, the client socket, which both left
			// backend reads unbounded and raced the uplink loop's deadline on
			// the same socket -- so the client teardown fired at unpredictable
			// intervals. Downlink is broker-paced and may be idle for long
			// stretches, so keep it generous; handleProxy's deferred
			// backendConn.Close() is what actually unblocks this loop.
			backendConn.SetReadDeadline(time.Now().Add(defaultProxyReadTimeout))

			backendPacket, err := packets.ReadPacket(backendReader)
			if err != nil {
				return
			}

			if pub, ok := backendPacket.(*packets.PublishPacket); ok {
				if n.logDownlink(conn, socketAddr, pub) {
					continue // self-echo: never forward a connection its own uplink
				}
			}

			var buf bytes.Buffer
			if err := backendPacket.Write(&buf); err != nil {
				n.Config.Log.Errorf("failed to serialize backend response packet: %v", err)
				return
			}

			if _, err := conn.Write(buf.Bytes()); err != nil {
				n.Config.Log.Errorf("failed to write backend response packet: %v", err)
				return
			}
		}
	}
}

// meshBroadcast is the Meshtastic broadcast NodeNum (^all).
const meshBroadcast = 0xffffffff

// logDownlink closes the proxy's last observability gap: every uplink packet is
// inspected and logged, but the downlink side logged nothing, so "the reply/ACK
// was published to the broker" and "the device never received it" were
// indistinguishable. Envelope metadata only, no decryption. Direct-to-node
// traffic (DMs, ACKs) is the interesting, low-volume signal and logs at Info;
// broadcast fan-out (NodeInfo/Position, every connected client gets a copy)
// logs at Debug to keep the firehose out of production logs.
//
// It also decides self-echo suppression (returns true = do not forward):
// mosquitto bounces a publish back to any subscriber of its own topic (MQTT
// 3.1.1 has no no-local), so a radio's own DMs come straight back down its BLE
// pipe -- wasted bandwidth on the flakiest link in the chain, and firmware
// would ignore them anyway.
//
// It is a thin wrapper over logDownlinkEnvelope, which holds the body: the v5
// downlink loop carries a paho.golang *Publish rather than this 3.1.1 type, and
// the only two fields either codec needs are the payload and the topic. Keeping
// this signature and this call site byte-unchanged (TestSelfEchoSuppression
// calls it directly and was not edited) is what makes the extraction provably
// behavior-preserving.
func (n *ServerCmd) logDownlink(conn net.Conn, socketAddr string, pub *packets.PublishPacket) (suppress bool) {
	return n.logDownlinkEnvelope(conn, socketAddr, pub.Payload, pub.TopicName)
}

// logDownlinkEnvelope is the codec-independent core of logDownlink: everything
// it decides comes from the raw PUBLISH payload and topic, so both the 3.1.1
// and the v5 downlink loops can call it. Returning true means "do not forward
// this packet to the client".
func (n *ServerCmd) logDownlinkEnvelope(conn net.Conn, socketAddr string, payload []byte, topic string) (suppress bool) {
	envelope := new(meshtastic.ServiceEnvelope)
	if err := proto.Unmarshal(payload, envelope); err != nil {
		return false
	}
	packet := envelope.GetPacket()
	if packet == nil {
		return false
	}

	from, to, id := packet.GetFrom(), packet.GetTo(), packet.GetId()

	if gw := envelope.GetGatewayId(); gw != "" && gw == n.gatewayFor(socketAddr) {
		n.Config.Log.Debugf("[proxy] DOWNLINK self-echo suppressed gw=%s from=!%08x to=!%08x id=%08x topic=%s client=%s",
			gw, from, to, id, topic, conn.RemoteAddr())
		return true
	}

	if to == meshBroadcast {
		n.Config.Log.Debugf("[proxy] DOWNLINK bcast from=!%08x id=%08x topic=%s client=%s",
			from, id, topic, conn.RemoteAddr())
		return false
	}

	kind := "channel"
	if packet.GetPkiEncrypted() {
		kind = "pki"
	}
	n.Config.Log.Infof("[proxy] DOWNLINK %s from=!%08x to=!%08x id=%08x topic=%s client=%s",
		kind, from, to, id, topic, conn.RemoteAddr())
	return false
}
