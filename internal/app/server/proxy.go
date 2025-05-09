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
		}
		conn.Close()
	}()

	socketAddr := n.TrackConnection(conn)

	backendAddress := n.Config.Server.ProxyForwardAddress
	backendConn, err := net.DialTimeout("tcp", backendAddress, 5*time.Second)
	if err != nil {
		n.Config.Log.Errorf("Failed to connect to backend MQTT broker: %v", err)
		return
	}
	defer backendConn.Close()

	request := bufio.NewReader(conn)
	backend := bufio.NewReader(backendConn)

	done := make(chan struct{})

	go n.handleBackend(conn, backend, done)

	for {
		select {
		case <-done:
			return

		default:
			packet, err := packets.ReadPacket(request)
			if err != nil {
				// Not a valuable log message because hangs and cutoffs happen.
				// if err != io.EOF {
				// 	n.Config.Log.Tracef("failed to read MQTT control packet from %s: %v", socketAddr, err)
				// }
				return
			}

			ip := &InspectorPacket{
				Log:   n.Config.Log,
				Track: &ConnectionInfo{SocketAddress: socketAddr},
				Raw:   &RawPacket{MQTT: &packet},
			}

			ip.inspectRawPacket(n)

			if ip.Meshtastic.WasUnmarshalled {
				n.Config.Log.Tracef("%s,%s,%s,%s,!%08x,!%08x", ip.Track.Username, ip.Track.ClientID, ip.Track.SocketAddress, ip.Meshtastic.PortNum, ip.Meshtastic.To, ip.Meshtastic.From)
			} else {
				n.Config.Log.Tracef("%s,%s,%s,%s", ip.Track.Username, ip.Track.ClientID, ip.Track.SocketAddress, ip.MQTT.Type)
			}

			result := n.Decider.Decide(ip)
			switch result.Decision {
			case Block:
				n.Config.Log.Debugf("BLOCK from %s: %s", ip.Track.ClientID, result.Reason)
				continue // Skip to next packet without forwarding
			case Allow:
				if strings.Contains(strings.ToLower(result.Reason), "no match") {
					n.Config.Log.Debugf("ALLOW from %s: %s: %+v", ip.Track.ClientID, result.Reason, *ip.Raw.MQTT)
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
				n.Config.Log.Errorf("failed to forward packet to backend: %v", err)
				return
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
			// if err != io.EOF && err != io.ErrUnexpectedEOF {
			// 	n.Config.Log.Errorf("Error reading from backend: %v", err)
			// }
			return
		}

		//TODO: Consider adding a mosquitto response interceptor here - not sure if this would ever be useful

		var buf bytes.Buffer
		if err := backendPacket.Write(&buf); err != nil {
			n.Config.Log.Errorf("Failed to serialize backend response packet: %v", err)
			return
		}

		if _, err := conn.Write(buf.Bytes()); err != nil {
			n.Config.Log.Errorf("Failed to write backend response to client: %v", err)
			return
		}
	}
}
