package server

import (
	"bufio"
	"bytes"
	"net"
	"strings"
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
	n.Config.Log.Debugf("Starting to handle connection from %s", socketAddr)

	// Get a backend connection from the pool
	backendConn, err := n.BackendPool.Get()
	if err != nil {
		n.Config.Log.Errorf("Failed to get backend connection from pool: %v", err)
		return
	}

	// Ensure the backend connection is returned to the pool when done
	defer func() {
		// Check if connection is still valid before returning to pool
		if backendConn != nil && backendConn.Conn != nil {
			n.BackendPool.Put(backendConn)
		}
	}()

	request := bufio.NewReader(conn)
	done := make(chan struct{})

	// Handle responses from the backend
	go n.handleBackend(conn, backendConn.Reader, done)

	for {
		select {
		case <-done:
			return

		default:
			// Update the read deadline for each packet
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))

			packet, err := packets.ReadPacket(request)
			if err != nil {
				//More like 'failed' - lots of ways this can legimately fail - not actually an error
				return
			}

			ip := &InspectorPacket{
				Log:   n.Config.Log,
				Track: &ConnectionInfo{SocketAddress: socketAddr},
				Raw:   &RawPacket{MQTT: &packet},
			}

			ip.inspectRawPacket(n)

			if ip.Meshtastic.WasUnmarshalled {
				n.Config.Log.Tracef("MQTT packet from %s: %s,%s,%s,!%08x,!%08x", socketAddr, ip.Track.Username, ip.Track.ClientID, ip.Meshtastic.PortNum, ip.Meshtastic.To, ip.Meshtastic.From)
			} else {
				n.Config.Log.Tracef("MQTT packet from %s: %s,%s,%s", socketAddr, ip.Track.Username, ip.Track.ClientID, ip.MQTT.Type)
			}

			// Apply decision rules
			result := n.Decider.Decide(ip)
			switch result.Decision {
			case Block:
				n.Config.Log.Debugf("BLOCK packet from %s: %s", ip.Track.ClientID, result.Reason)
				continue // Skip to next packet without forwarding
			case Allow:
				if strings.Contains(strings.ToLower(result.Reason), "no match") {
					n.Config.Log.Debugf("ALLOW packet from %s: %s", ip.Track.ClientID, result.Reason)
				}
			}

			// Serialize the packet for forwarding
			var buf bytes.Buffer
			if err := (*ip.Raw.MQTT).Write(&buf); err != nil {
				n.Config.Log.Errorf("Failed to serialize MQTT packet: %v", err)
				return
			}

			// Forward the packet to the backend
			if _, err := backendConn.Conn.Write(buf.Bytes()); err != nil {
				newBackendConn, err := n.BackendPool.Get()
				if err != nil {
					n.Config.Log.Errorf("Failed to get replacement backend connection: %v", err)
					return
				}

				// Replace the old connection
				oldBackendConn := backendConn
				backendConn = newBackendConn

				// Start a new goroutine to handle responses from the new backend connection
				oldDone := done
				done = make(chan struct{})
				go n.handleBackend(conn, backendConn.Reader, done)

				// Try to send the packet again with the new connection
				if _, err := backendConn.Conn.Write(buf.Bytes()); err != nil {
					n.Config.Log.Errorf("Failed to forward packet with replacement connection: %v", err)
					return
				}

				go func() {
					<-oldDone
					if oldBackendConn.Conn != nil {
						oldBackendConn.Conn.Close()
					}
				}()
			}
		}
	}
}

func (n *ServerCmd) handleBackend(conn net.Conn, backendReader *bufio.Reader, done chan struct{}) {

	defer func() {
		done <- struct{}{}
	}()

	for {
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
