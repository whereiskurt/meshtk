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
	startTime := time.Now()

	// Track metrics for this connection
	var packetCount int

	defer func() {
		if conn.RemoteAddr() != nil {
			socketAddr := conn.RemoteAddr().String()
			n.ConnMutex.Lock()
			delete(n.ConnTrack, socketAddr)
			n.ConnMutex.Unlock()
			conn.Close()
		}

		// Log connection summary on close
		duration := time.Since(startTime)
		n.Config.Log.Tracef("Connection handled %d packets over %v before closing", packetCount, duration)
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
			n.Config.Log.Debugf("Returning backend connection to pool from %s session", socketAddr)
			n.BackendPool.Put(backendConn)
		}
	}()

	request := bufio.NewReader(conn)
	done := make(chan struct{})

	// Handle responses from the backend
	go n.handleBackend(conn, backendConn.Reader, done)

	// Set a reasonable read timeout to avoid stuck connections
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	for {
		select {
		case <-done:
			n.Config.Log.Debugf("Backend connection signaled completion for %s", socketAddr)
			return

		default:
			// Update the read deadline for each packet
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))

			packet, err := packets.ReadPacket(request)
			if err != nil {
				n.Config.Log.Debugf("Error reading packet from %s: %v", socketAddr, err)
				return
			}
			packetCount++
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
				n.Config.Log.Debugf("BLOCK packet #%d from %s: %s", packetCount, ip.Track.ClientID, result.Reason)
				continue // Skip to next packet without forwarding
			case Allow:
				if strings.Contains(strings.ToLower(result.Reason), "no match") {
					n.Config.Log.Debugf("ALLOW packet #%d from %s: %s", packetCount, ip.Track.ClientID, result.Reason)
				}
			}

			// Serialize the packet for forwarding
			var buf bytes.Buffer
			if err := (*ip.Raw.MQTT).Write(&buf); err != nil {
				n.Config.Log.Errorf("Failed to serialize MQTT packet #%d: %v", packetCount, err)
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
	var packetCount int
	startTime := time.Now()

	defer func() {
		done <- struct{}{}
		n.Config.Log.Tracef("Backend handler processed %d packets over %v before closing", packetCount, time.Since(startTime))
	}()

	for {
		backendPacket, err := packets.ReadPacket(backendReader)
		if err != nil {
			return
		}

		packetCount++

		var buf bytes.Buffer
		if err := backendPacket.Write(&buf); err != nil {
			n.Config.Log.Errorf("Failed to serialize backend response packet #%d: %v", packetCount, err)
			return
		}

		if _, err := conn.Write(buf.Bytes()); err != nil {
			n.Config.Log.Errorf("Failed to write backend response packet #%d to client: %v", packetCount, err)
			return
		}

	}
}
