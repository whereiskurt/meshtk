package server

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

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

	// Create a direct connection to the backend instead of using a pool
	backendConn, err := net.DialTimeout("tcp", n.Config.Server.ProxyForwardAddress, 10*time.Second)
	if err != nil {
		n.Config.Log.Errorf("failed to connect to backend: %v", err)
		return
	}
	defer backendConn.Close()

	request := bufio.NewReader(conn)
	backendReader := bufio.NewReader(backendConn)
	done := make(chan struct{})

	// Create a context with cancel for proper goroutine cleanup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure we cancel the context when this function exits

	// Handle responses from the backend
	go n.handleBackend(ctx, conn, backendReader, done)

	for {
		select {
		case <-done:
			n.Config.Log.Debugf("Connection from %s closed by backend", socketAddr)
			return
		default:
			// Update the read deadline for each packet
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))

			packet, err := packets.ReadPacket(request)
			if err != nil {
				n.Config.Log.Debugf("Connection from %s closed: %v", socketAddr, err)
				return
			}

			ip := &InspectorPacket{
				Log:   n.Config.Log,
				Track: &ConnectionInfo{SocketAddress: socketAddr},
				Raw:   &RawPacket{MQTT: &packet},
			}

			ip.inspectRawPacket(n)

			// Apply decision rules
			result := n.PacketDecider.Decide(ip)

			switch result.Decision {
			//TODO: Add the idea of a "block" log instead of just logging to the config log
			case Allow:
				if n.Config.Server.ShouldWriteAllows {
					n.Config.Log.Infof("%s", ip.FormattedLog(result))
				}
			case Block:
				if n.Config.Server.ShouldWriteBlocks {
					n.Config.Log.Infof("%s", ip.FormattedLog(result))
				}
				return
			default:
				if n.Config.Server.ShouldWriteAllows || n.Config.Server.ShouldWriteBlocks {
					n.Config.Log.Infof("%s", ip.FormattedLog(result))
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
				n.Config.Log.Errorf("Failed to write to backend: %v", err)
				return
			}
		}
	}
}

func (n *ServerCmd) handleBackend(ctx context.Context, conn net.Conn, backendReader *bufio.Reader, done chan struct{}) {

	defer func() {
		done <- struct{}{}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))

			backendPacket, err := packets.ReadPacket(backendReader)
			if err != nil {
				n.Config.Log.Debugf("Backend connection error: %v", err)
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
