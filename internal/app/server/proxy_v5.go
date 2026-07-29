package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"time"

	v5 "github.com/eclipse/paho.golang/packets"
)

// maxV5PacketBytes caps how large a v5 control packet the proxy will buffer.
// The remaining-length field is attacker-controlled and up to 4 varint bytes
// wide, so an uncapped make([]byte, remLen) lets five bytes on the wire buy a
// 256 MiB allocation. 256 KiB is already generous: mosquitto enforces
// message_size_limit 1024 on payloads and Meshtastic topics are ~30 bytes.
//
// The cap deliberately applies to the v5 path ONLY. The 3.1.1 codec has always
// allocated unbounded here, and this phase may not change 3.1.1 behavior.
const maxV5PacketBytes = 256 * 1024

// readFrame reads exactly one MQTT control packet off the wire as raw bytes,
// interpreting ONLY the fixed-header framing: one type/flags byte, a 1..4 byte
// varint remaining length, then that many body bytes. That framing is identical
// in 3.1.1 and v5, so the captured bytes can be relayed verbatim OR handed to a
// codec — and, crucially, a packet the codec cannot parse is still forwardable.
//
// That last property is why the v5 path captures before it parses. paho.golang
// cannot parse a zero-length DISCONNECT (e000, legal per MQTT 5.0 §3.14.2.1 and
// common in the wild — it returns EOF), it re-encodes a short PUBACK 40021234
// into the longer 400412340000, and Properties.Unpack hard-errors on any
// property id it does not model. Parsing every packet would turn each of those
// into a torn-down connection; relaying captured frames makes them non-events.
func readFrame(r *bufio.Reader) (raw []byte, pktType byte, err error) {
	b0, err := r.ReadByte()
	if err != nil {
		return nil, 0, err
	}

	frame := []byte{b0}
	var remLen int
	var mult uint32
	for i := 0; ; i++ {
		if i == 4 {
			return nil, 0, fmt.Errorf("malformed remaining length")
		}
		d, err := r.ReadByte()
		if err != nil {
			return nil, 0, err
		}
		frame = append(frame, d)
		remLen |= int(d&0x7F) << mult
		if d&0x80 == 0 {
			break
		}
		mult += 7
	}

	// Check BEFORE allocating -- that ordering is the whole mitigation.
	if remLen > maxV5PacketBytes {
		return nil, 0, fmt.Errorf("v5 packet too large: %d bytes", remLen)
	}

	if remLen > 0 {
		body := make([]byte, remLen)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, 0, err
		}
		frame = append(frame, body...)
	}

	return frame, b0 >> 4, nil
}

// writeMqtt5Connack answers a v5 client with a bare CONNACK carrying the given
// reason code and no properties. Byte-identical to the hand-rolled literal in
// writeMqtt5UnsupportedConnack for 0x84 (pinned by test), but immune to
// reason-code typos and self-documenting for 0x87 / 0x8C.
func writeMqtt5Connack(conn net.Conn, reason byte) error {
	ca := v5.NewControlPacket(v5.CONNACK)
	ca.Content.(*v5.Connack).ReasonCode = reason
	_, err := ca.WriteTo(conn)
	return err
}

// handleProxyV5 is the client->backend read loop for MQTT 5.0 connections. It
// is a sibling of handleProxy, not a modification of it: the 3.1.1 body must
// stay byte-for-byte unchanged, so the version decision made at the preflight
// dispatches into a whole separate handler rather than threading a codec
// abstraction through the existing loops.
//
// Only CONNECT is parsed here. Everything else is relayed as captured bytes;
// v5 PUBLISH inspection lands in plan 68-02 and fails closed until it does.
func (n *ServerCmd) handleProxyV5(conn net.Conn, request *bufio.Reader, socketAddr string) {
	// The preflight peek did not consume anything, so the CONNECT is still
	// queued on the reader.
	conn.SetReadDeadline(time.Now().Add(defaultProxyReadTimeout))
	frame, pktType, err := readFrame(request)
	if err != nil {
		n.InspectorLogger.Warnf("action=MQTT5_PARSE_FAIL, ip=%s, reason=%v", socketAddr, err)
		return
	}
	if pktType != v5.CONNECT {
		n.InspectorLogger.Warnf("action=MQTT5_PARSE_FAIL, ip=%s, reason=first packet is type %d, not CONNECT", socketAddr, pktType)
		return
	}

	// Parse a COPY of the captured bytes -- never read the socket through the
	// codec, or the raw frame is gone if parsing fails.
	cp, err := v5.ReadPacket(bytes.NewReader(frame))
	if err != nil {
		// Fail closed: an unparseable CONNECT cannot be authenticated, so the
		// backend is never dialed.
		n.InspectorLogger.Warnf("action=MQTT5_PARSE_FAIL, ip=%s, reason=%v", socketAddr, err)
		return
	}
	c, ok := cp.Content.(*v5.Connect)
	if !ok {
		n.InspectorLogger.Warnf("action=MQTT5_PARSE_FAIL, ip=%s, reason=CONNECT frame parsed as %T", socketAddr, cp.Content)
		return
	}

	// Same keepalive -> read-deadline mapping the 3.1.1 path uses. Only this
	// goroutine ever touches the CLIENT socket's deadline: handleBackend used
	// to clobber it from the downlink side and tore down live radios.
	readTimeout := proxyReadTimeout(c.KeepAlive)

	// inspectV5Connect answers the client with the version-correct CONNACK on
	// every rejection path, so a false return means "done, say nothing more".
	if !n.inspectV5Connect(conn, socketAddr, c) {
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
	defer cancel()

	// The v5 uplink loop spawns the v5 downlink loop directly, so the protocol
	// version reaches handleBackendV5 by construction. A ConnTrack lookup would
	// race: the entry is not created until the CONNECT is inspected, but the
	// downlink goroutine starts before that and would miss the CONNACK.
	go n.handleBackendV5(ctx, conn, socketAddr, backendConn, backendReader)

	// Forward the RE-ENCODED CONNECT, never the captured frame -- the captured
	// frame still carries the client's own credentials.
	var out bytes.Buffer
	if _, err := cp.WriteTo(&out); err != nil {
		n.Config.Log.Errorf("failed to serialize v5 CONNECT: %v", err)
		return
	}
	if _, err := backendConn.Write(out.Bytes()); err != nil {
		n.Config.Log.Errorf("failed to write v5 CONNECT to backend: %v", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
			conn.SetReadDeadline(time.Now().Add(readTimeout))

			frame, pktType, err := readFrame(request)
			if err != nil {
				backendConn.Close()
				return
			}

			if pktType == v5.PUBLISH {
				// Fail closed until plan 68-02 wires the v5 envelope inspection:
				// an uninspected PUBLISH must not reach the broker, because the
				// hop clamp, the rules engine and the payload rewrites all hang
				// off that path.
				n.InspectorLogger.Warnf("action=BLOCK, ip=%s, reason=v5_publish_inspection_pending", socketAddr)
				return
			}

			// Everything else -- SUBSCRIBE, PUBACK, PINGREQ, DISCONNECT, AUTH,
			// anything carrying an unmodeled property -- goes out exactly as it
			// came in.
			if _, err := backendConn.Write(frame); err != nil {
				n.Config.Log.Errorf("failed to write to backend: %v", err)
				return
			}
		}
	}
}

// handleBackendV5 is the backend->client read loop for MQTT 5.0 connections,
// mirroring handleBackend. The read deadline goes on the socket being READ
// (backendConn) and never on conn -- deadlining the client socket from here is
// the exact bug the comments in handleBackend record.
func (n *ServerCmd) handleBackendV5(ctx context.Context, conn net.Conn, socketAddr string, backendConn net.Conn, backendReader *bufio.Reader) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			backendConn.SetReadDeadline(time.Now().Add(defaultProxyReadTimeout))

			frame, _, err := readFrame(backendReader)
			if err != nil {
				return
			}

			// Raw relay in both directions for now. CONNACK gains a
			// topic-alias strip in the next commit; downlink PUBLISH gains
			// self-echo suppression in plan 68-02.
			if _, err := conn.Write(frame); err != nil {
				n.Config.Log.Errorf("failed to write backend response packet: %v", err)
				return
			}
		}
	}
}
