package server

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	"github.com/whereiskurt/meshtk/pkg/network"
)

var rateLimiter = network.NewLimiter(
	5,             // 5 tokens per second
	10,            // allow a burst of 10
	2*time.Minute, // forget after 10 minutes of inactivity
	15,            // if more than 15 missing tokens -> kill
)

const socketPenalty = time.Duration(200) * time.Millisecond

func (n *ServerCmd) handleProxy(conn net.Conn) {

	defer func() {
		if conn.RemoteAddr() != nil {
			socketAddr := conn.RemoteAddr().String()
			n.ConnMutex.Lock()
			delete(n.ConnTrack, socketAddr)
			n.ConnMutex.Unlock()
			conn.Close()
		}
	}()

	socketAddr := n.TrackConnection(conn)

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

	go n.handleBackend(ctx, conn, backendReader)

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// Update the read deadline for each packet
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))

			packet, err := packets.ReadPacket(request)
			if err != nil {
				return
			}

			ip := &InspectorPacket{
				Log:   n.InspectorLogger,
				Track: &ConnectionInfo{SocketAddress: socketAddr},
				Raw:   &RawPacket{MQTT: &packet},
			}

			ip.inspectRawPacket(n)

			// Apply decision rules
			result := n.PacketDecider.Decide(ip)

			// slowed, kill, tokenCount := rateLimiter.CheckLimit(socketAddr)
			slowed, kill, tokenCount := rateLimiter.EnforceLimit(socketAddr, socketPenalty)
			penalty := min(socketPenalty*time.Duration(-tokenCount), 10*time.Second)

			if slowed {
				ip.WriteLimiterLog(Slow, tokenCount, penalty)
			} else if kill {
				ip.WriteLimiterLog(Kill, tokenCount, penalty)
				conn.Close()
				backendConn.Close()
				return
			}

			switch result.Decision {
			case Allow:
				if n.Config.Server.ShouldLogAllows {
					ip.WriteDecisionLog(result)
				}
			case Block:
				if n.Config.Server.ShouldLogBlocks {
					ip.WriteDecisionLog(result)
				}
				return
			default:
				if n.Config.Server.ShouldLogAllows || n.Config.Server.ShouldLogBlocks {
					ip.WriteDecisionLog(result)
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

func (n *ServerCmd) handleBackend(ctx context.Context, conn net.Conn, backendReader *bufio.Reader) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))

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
