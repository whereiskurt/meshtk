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

func (n *ServerCmd) handleProxy(conn net.Conn) {
	socketAddr := n.TrackConnection(conn)

	defer func(conn net.Conn, socketAddr string) {
		n.ConnMutex.Lock()
		delete(n.ConnTrack, socketAddr)
		n.ConnMutex.Unlock()
		conn.Close()
	}(conn, socketAddr)

	backendConn, err := net.DialTimeout("tcp", n.Config.Server.ProxyForwardAddress, 10*time.Second)
	if err != nil {
		n.Config.Log.Errorf("failed to connect to backend: %v", err)
		return
	}
	defer backendConn.Close()

	request := bufio.NewReader(conn)
	backendReader := bufio.NewReader(backendConn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure we cancel the context when this function exits

	go n.handleBackend(ctx, conn, backendConn, backendReader)

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

func (n *ServerCmd) handleBackend(ctx context.Context, conn net.Conn, backendConn net.Conn, backendReader *bufio.Reader) {
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
