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

// writeMqtt5Disconnect answers a v5 client with a bare DISCONNECT carrying the
// given reason code and no properties, shaped exactly like writeMqtt5Connack.
//
// DISCONNECT and not a second CONNACK: a CONNACK is only legal in response to a
// CONNECT on a fresh session (MQTT 5.0 3.2), so the spec-correct way to answer a
// mid-session protocol violation is to tell the client why and close.
func writeMqtt5Disconnect(conn net.Conn, reason byte) error {
	d := v5.NewControlPacket(v5.DISCONNECT)
	d.Content.(*v5.Disconnect).ReasonCode = reason
	_, err := d.WriteTo(conn)
	return err
}

// handleProxyV5 is the client->backend read loop for MQTT 5.0 connections. It
// is a sibling of handleProxy, not a modification of it: the 3.1.1 body must
// stay byte-for-byte unchanged, so the version decision made at the preflight
// dispatches into a whole separate handler rather than threading a codec
// abstraction through the existing loops.
//
// Every frame type has a stated outcome: inspected (CONNECT, PUBLISH), refused
// (the credential/auth-bearing and server-only types), or relayed as captured
// bytes. Nothing falls through.
//
// The PUBLISH posture is FAIL CLOSED, and it is worth stating here because a
// reader auditing the security model should not have to find it 100 lines down.
// A PUBLISH the codec cannot parse is hand-parsed and inspected anyway
// (proxy_v5_rawpublish.go); a PUBLISH nothing can read at all is refused and the
// connection ends, exactly as the 3.1.1 loop ends a connection on a packet its
// codec cannot read. Relaying an uninspected PUBLISH was CR-04 -- the retired
// accepted risk T-68-02-06 -- and it is not the posture any more.
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

			// EVERY frame refreshes the tracker entry, before any type
			// dispatch -- the 3.1.1 loop does the same by calling SetConnTrack
			// in every inspectRawPacket branch, keepalives included. The
			// reaper purges anything idle for 180s, which is far shorter than
			// the Meshtastic publish cadence (position is ~15 min), so without
			// this line a live session loses its entry between publishes and
			// the next PUBLISH is Blocked with "Username required for MQTT".
			n.touchConnTrack(socketAddr)

			switch pktType {
			case v5.PUBLISH:
				// Inspection, rules, rewrites and forwarding all happen inside;
				// a false return means the connection must be dropped, exactly
				// as handleProxy returns on a Block decision.
				if !n.handleV5PublishUplink(backendConn, socketAddr, frame) {
					return
				}

			case v5.SUBSCRIBE:
				// Parse a COPY of the captured bytes, never the socket. The
				// parse is READ-ONLY and the CAPTURED frame is what gets
				// relayed: re-encoding would risk the same
				// subscription-identifier round-trip hazard that keeps the
				// downlink path from re-encoding.
				sp, perr := v5.ReadPacket(bytes.NewReader(frame))
				if perr != nil {
					// Relay, do not close. A SUBSCRIBE carries no credentials
					// and no topic Block rule exists today, so a loud relay
					// beats tearing down a live session over one unmodelled
					// property. Accepted risk T-68-06-05.
					n.InspectorLogger.Warnf("action=MQTT5_PARSE_FAIL, ip=%s, mqtt_type=SUBSCRIBE, reason=%v", socketAddr, perr)
					if !n.writeToBackend(backendConn, frame) {
						return
					}
					continue
				}

				ip := n.inspectV5Subscribe(socketAddr, sp)
				result := n.PacketDecider.Decide(ip)
				switch result.Decision {
				case Allow:
					if n.Config.Server.ShouldLogAllows {
						ip.WriteDecisionLog(result)
					}
				case Block:
					if n.Config.Server.ShouldLogBlocks {
						ip.WriteDecisionLog(result)
					}
					n.Config.Log.Warnf("[proxy] BLOCK subscribe topics=%v reason=%q user=%s ip=%s",
						ip.MQTT.Topics, result.Reason, ip.Track.Username, ip.Track.SocketAddress)
					return
				default:
					if n.Config.Server.ShouldLogAllows || n.Config.Server.ShouldLogBlocks {
						ip.WriteDecisionLog(result)
					}
				}

				if !n.writeToBackend(backendConn, frame) {
					return
				}

			case v5.CONNECT, v5.AUTH, v5.CONNACK, v5.SUBACK, v5.UNSUBACK, v5.PINGRESP:
				// Refused, and refused BEFORE anything is written to the
				// broker. A second CONNECT and an AUTH frame both carry the
				// client's own credentials or auth state -- relaying either
				// hands mosquitto exactly what re-encoding the establishing
				// CONNECT exists to withhold, and inspect_v5.go's enhanced-auth
				// branch asserts an AUTH packet never enters an authenticated
				// session. CONNACK/SUBACK/UNSUBACK/PINGRESP are server-to-client
				// only; a client sending one is broken or probing.
				n.InspectorLogger.Warnf("action=MQTT5_PROTOCOL_VIOLATION, ip=%s, mqtt_type=%d, reason=illegal_frame_on_established_session",
					socketAddr, pktType)
				writeMqtt5Disconnect(conn, v5.DisconnectProtocolError)
				return

			default:
				// PUBACK, PUBREC, PUBREL, PUBCOMP, SUBSCRIBE, UNSUBSCRIBE,
				// PINGREQ, DISCONNECT -- and any type this codec does not model
				// -- relay exactly as captured. Forwarding unknown types rather
				// than dropping them is a locked phase decision; the point of
				// the switch is that it is now stated rather than a fallthrough.
				if _, err := backendConn.Write(frame); err != nil {
					n.Config.Log.Errorf("failed to write to backend: %v", err)
					return
				}
			}
		}
	}
}

// handleV5PublishUplink inspects one captured v5 PUBLISH frame and forwards it
// to the broker, mirroring handleProxy's decide/forward sequence. It returns
// false when the connection must be dropped -- a Block decision or a dead
// backend -- which is what handleProxy's `return` does on the 3.1.1 path.
func (n *ServerCmd) handleV5PublishUplink(backendConn net.Conn, socketAddr string, frame []byte) bool {
	// Parse a COPY of the captured bytes, never the socket, so the raw frame
	// survives a parse failure.
	cp, err := v5.ReadPacket(bytes.NewReader(frame))
	if err != nil {
		// The parse-fail line stays: it is the signal that tells ops this path
		// was taken at all, and 68-08 watches its rate in prod.
		n.InspectorLogger.Warnf("action=MQTT5_PARSE_FAIL, ip=%s, mqtt_type=PUBLISH, reason=%v", socketAddr, err)

		// This branch used to relay the frame and return true, which skipped --
		// in order -- the topic-alias guard, the inspector, PacketDecider.Decide,
		// RewriteHopLimit, BlockInvalidEncryption and every Block rule. Since
		// paho.golang hard-errors on ANY property id outside its table, three
		// client-chosen bytes bought a permanent inspection exemption from the
		// control that exists to stop fleet-wide RF flood amplification. That was
		// accepted risk T-68-02-06; verification reproduced it as PROBE-A and it
		// is now closed by hand-parsing the frame instead.
		//
		// PUBLISH is a HANDLED type, so this does not contradict the locked
		// "unknown packet types forward raw" decision -- that governs the types
		// the proxy has no reason to inspect, and they still relay in the frame
		// switch's default arm.
		rp, perr := parseV5PublishFrame(frame)
		if perr != nil {
			// Fail closed. Only a frame whose own length prefixes disagree with
			// its bytes reaches here, which is a frame mosquitto would refuse
			// too, and the 3.1.1 loop ends the connection on any packet its
			// codec cannot read. One line per socket, since the socket closes.
			n.InspectorLogger.Warnf("action=MQTT5_PUBLISH_HEADER_FAIL, ip=%s, reason=%v", socketAddr, perr)
			n.Config.Log.Warnf("[proxy] BLOCK from=!%08x to=!%08x reason=%q user=%s ip=%s",
				0, 0, "unreadable v5 PUBLISH header", "", socketAddr)
			return false
		}

		// Same guard, same reason as the parseable path: a blank topic blinds
		// every topic rule and every msh/... log line while the broker resolves
		// the alias and fans the packet out perfectly normally.
		if rp.Topic == "" {
			n.InspectorLogger.Warnf("action=BLOCK, ip=%s, reason=topic_alias_uplink", socketAddr)
			return false
		}

		ip := n.inspectV5RawPublish(socketAddr, rp)
		if !n.decideV5Publish(ip) {
			return false
		}

		// Forward exactly once, same contract as below. The splice preserves the
		// property bytes the codec refused to parse, which a re-encode could not;
		// a splice failure Blocks and drops rather than quietly forwarding the
		// unclamped original.
		out := frame
		if ip.WireRewritten {
			spliced, serr := spliceV5PublishPayload(frame, rp, rp.Payload)
			if serr != nil {
				n.Config.Log.Errorf("failed to splice rewritten v5 PUBLISH payload: %v", serr)
				return false
			}
			out = spliced
		}
		return n.writeToBackend(backendConn, out)
	}
	p, ok := cp.Content.(*v5.Publish)
	if !ok {
		n.InspectorLogger.Warnf("action=MQTT5_PARSE_FAIL, ip=%s, mqtt_type=PUBLISH, reason=PUBLISH frame parsed as %T", socketAddr, cp.Content)
		return n.writeToBackend(backendConn, frame)
	}

	// Topic-alias guard. 68-01 already makes aliasing impossible by stripping
	// TopicAliasMaximum from both the CONNECT and the CONNACK, so reaching here
	// means a spec-violating client. Refuse loudly: a blank topic would blind
	// every topic rule and every msh/... log line while mosquitto resolved the
	// alias and fanned the packet out perfectly normally.
	if (p.Properties != nil && p.Properties.TopicAlias != nil) || p.Topic == "" {
		n.InspectorLogger.Warnf("action=BLOCK, ip=%s, reason=topic_alias_uplink", socketAddr)
		return false
	}

	ip := n.inspectV5Publish(socketAddr, cp)
	if !n.decideV5Publish(ip) {
		return false
	}

	// Forward EXACTLY ONCE, and the choice is explicit. A rule that mutated the
	// packet set WireRewritten, so the re-encode is what goes out (topic, QoS
	// bits, packet id and the whole properties block all survive the round
	// trip). Nothing mutated it, so the captured bytes go out untouched.
	// Writing both -- or mutating the struct and forwarding the original frame
	// -- is precisely the silent no-op meshtk#22 shipped.
	out := frame
	if ip.WireRewritten {
		var buf bytes.Buffer
		if _, err := cp.WriteTo(&buf); err != nil {
			n.Config.Log.Errorf("failed to serialize v5 PUBLISH: %v", err)
			return false
		}
		out = buf.Bytes()
	}

	return n.writeToBackend(backendConn, out)
}

// decideV5Publish runs the decide/log sequence for one v5 uplink PUBLISH and
// reports whether the connection may continue, mirroring handleProxy's 3.1.1
// sequence. It is shared by the codec-parsed and hand-parsed paths deliberately:
// two copies of this switch would be two chances for the exempt path to drift
// back into a quieter, softer decision than the one the fleet is judged by.
func (n *ServerCmd) decideV5Publish(ip *InspectorPacket) bool {
	result := n.PacketDecider.Decide(ip)
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
		return false
	default:
		if n.Config.Server.ShouldLogAllows || n.Config.Server.ShouldLogBlocks {
			ip.WriteDecisionLog(result)
		}
	}
	return true
}

// writeToBackend relays a frame and reports whether the connection is still
// usable, so callers can `return false` on a dead backend without repeating the
// error-log boilerplate.
func (n *ServerCmd) writeToBackend(backendConn net.Conn, frame []byte) bool {
	if _, err := backendConn.Write(frame); err != nil {
		n.Config.Log.Errorf("failed to write to backend: %v", err)
		return false
	}
	return true
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

			frame, pktType, err := readFrame(backendReader)
			if err != nil {
				return
			}

			// CONNACK is the ONLY downlink packet parsed here, and only to strip
			// the broker's topic-alias budget: mosquitto 2.0 advertises
			// TopicAliasMaximum=10 by default, and a client holding a non-zero
			// budget may publish with an EMPTY topic plus a Topic Alias
			// property. mosquitto would resolve the alias and fan the packet out
			// normally while every topic-based rule and every msh/... log line
			// in this proxy went blind -- silently, which is what makes it
			// dangerous.
			//
			// On a parse failure the captured frame goes out unchanged:
			// forwarding a CONNACK the codec cannot model beats closing a
			// connection the broker just accepted.
			if pktType == v5.CONNACK {
				if pk, perr := v5.ReadPacket(bytes.NewReader(frame)); perr == nil {
					if ca, ok := pk.Content.(*v5.Connack); ok && ca.Properties != nil {
						ca.Properties.TopicAliasMaximum = nil
						var out bytes.Buffer
						if _, werr := pk.WriteTo(&out); werr == nil {
							frame = out.Bytes()
						}
					}
				}
			}

			// PUBLISH is parsed READ-ONLY and the captured frame is what gets
			// written. Never re-encode downlink: Properties.SubscriptionIdentifier
			// is modelled as a single pointer while MQTT 5.0 permits several on
			// one PUBLISH (overlapping subscriptions), so a round trip would
			// silently drop all but one. The only fields needed here are the
			// payload and the topic.
			if pktType == v5.PUBLISH {
				pk, perr := v5.ReadPacket(bytes.NewReader(frame))
				if perr != nil {
					// Relay, do not close -- same reasoning as the uplink side.
					n.InspectorLogger.Warnf("action=MQTT5_PARSE_FAIL, ip=%s, mqtt_type=PUBLISH_DOWNLINK, reason=%v",
						socketAddr, perr)
				} else if p, ok := pk.Content.(*v5.Publish); ok {
					// Suppression is simply "do not write the frame": mosquitto
					// bounces a publish back to any subscriber of its own topic
					// (there is no no-local here), so a radio's own DMs come
					// straight back down its BLE pipe -- pure waste on the
					// flakiest link in the chain, and firmware ignores them.
					if n.logDownlinkV5(conn, socketAddr, p) {
						continue
					}
				}
			}

			if _, err := conn.Write(frame); err != nil {
				n.Config.Log.Errorf("failed to write backend response packet: %v", err)
				return
			}
		}
	}
}
