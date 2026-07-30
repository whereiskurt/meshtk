package server

import (
	"encoding/binary"
	"fmt"
)

// v5RawSubscribe is a hand-parsed view of one MQTT 5.0 SUBSCRIBE frame,
// produced WITHOUT interpreting a single property id.
//
// It exists because paho.golang's Properties.Unpack hard-errors on any property
// id outside its table, and until this file landed a v5.ReadPacket failure made
// the v5 SUBSCRIBE arm relay the frame WITHOUT EVER BUILDING AN InspectorPacket
// -- so it never reached PacketDecider, MQTT.Topics was never recorded, and the
// first topic Block rule anyone added would silently not apply to it. Three
// client-chosen bytes in the property block therefore bought an exemption from
// inspection one layer up from CR-04, bought with the SAME three bytes
// (68-REVIEW WR-04, accepted risk T-68-06-05).
//
// This is the sibling of v5RawPublish and is deliberately shaped like it. The
// one thing it does NOT carry is an alias scan: a SUBSCRIBE has no topic alias,
// so there is nothing for a scan to find and no reason for an id table here.
type v5RawSubscribe struct {
	// PacketID is the two-byte identifier every SUBSCRIBE carries. Reported so
	// the view is a complete account of the variable header, not so anything
	// judges it.
	PacketID uint16
	// Filters holds the topic filters in WIRE ORDER, which is the order
	// inspectV5Subscribe records them in and therefore the order a topic rule
	// sees on the codec path. Order is part of the parity claim.
	//
	// REPORTED, never judged -- exactly like v5RawPublish.Topic. An empty
	// filter string is a policy question for a rule; the parser has no business
	// answering it. An empty filter LIST is different: MQTT 5.0 3.8.3 requires
	// at least one, so that is a framing error and parseV5SubscribeFrame
	// refuses it.
	Filters []string
}

// parseV5SubscribeFrame reads the packet identifier and the topic filters out of
// a captured MQTT 5.0 SUBSCRIBE frame using only what the wire format guarantees
// is skippable without knowing what any property means:
//
//	byte 0            : 0x8<<4 | 0x2 (SUBSCRIBE, reserved flags)
//	varint            : remaining length (1..4 bytes)
//	uint16            : packet identifier -- ALWAYS present on a SUBSCRIBE
//	varint            : property block length in bytes
//	n bytes           : property block -- SKIPPED WHOLE by that length
//	repeated:
//	  uint16 + n bytes: topic filter
//	  1 byte          : subscription options
//
// IT READS NO PROPERTY ID, AND THAT IS THE POINT -- it is the CR-04 lesson
// applied before the mistake is made. CR-04 was that a property table in the
// inspection path let a client choose its own judge: paho.golang hard-errors on
// an id outside its table, the error skipped inspection, and three bytes bought
// an exemption. A table here would rebuild that lever. The block is skipped as a
// UNIT by its own declared length, so the filters' position is computable from
// length prefixes alone and a property id this proxy has never heard of costs
// exactly nothing. That claim is falsifiable and is tested behaviorally:
// TestParseV5SubscribeFrameIsPropertyAgnostic feeds two frames differing in ONE
// byte -- the property id, one modelled and one no MQTT 5.0 table defines -- and
// requires an identical packet id and an identical filter list.
//
// It returns an error and NO PARTIAL VIEW on any inconsistency: a length prefix
// that does not fit, a remaining length that disagrees with the bytes actually
// present, a filter with no subscription-options byte after it, a walk that does
// not end exactly at the frame end, or an empty filter list. Same discipline as
// parseV5PublishFrame, and the same bar: a frame whose own length prefixes
// contradict its bytes is one mosquitto would refuse too.
//
// ONE ASYMMETRY, STATED RATHER THAN HIDDEN. paho.golang accepts a SUBSCRIBE with
// an empty subscription list (probed, not assumed -- see
// TestV5CodecParseableEmptyFilterListStillRelays), so such a frame goes down the
// CODEC path and is relayed, while the same frame carrying an unmodelled
// property id arrives here and is refused. That is deliberate: MQFX-04 asks for
// inspection to stop depending on which parser succeeded, not for the proxy to
// start refusing frames the codec reads fine. mosquitto refuses an empty
// SUBSCRIBE either way, so the frame dies one hop later rather than never.
func parseV5SubscribeFrame(frame []byte) (*v5RawSubscribe, error) {
	if len(frame) < 2 {
		return nil, fmt.Errorf("frame too short: %d bytes", len(frame))
	}
	if frame[0]>>4 != 8 {
		return nil, fmt.Errorf("not a SUBSCRIBE: fixed-header type %d", frame[0]>>4)
	}

	// Same varint decode and same 4-byte termination guard as readFrame.
	remLen, n, err := decodeV5Varint(frame[1:])
	if err != nil {
		return nil, fmt.Errorf("remaining length: %w", err)
	}
	// Check the cap BEFORE trusting the length for anything -- the same
	// check-before-you-act ordering readFrame uses before it allocates.
	if remLen > maxV5PacketBytes {
		return nil, fmt.Errorf("v5 packet too large: %d bytes", remLen)
	}

	pos := 1 + n
	end := pos + remLen
	if end != len(frame) {
		return nil, fmt.Errorf("remaining length %d disagrees with the %d bytes present", remLen, len(frame)-pos)
	}

	// The packet identifier is unconditional on a SUBSCRIBE -- unlike a PUBLISH,
	// where its presence is decided by the QoS bits in the fixed header.
	if pos+2 > end {
		return nil, fmt.Errorf("truncated packet identifier")
	}
	packetID := binary.BigEndian.Uint16(frame[pos : pos+2])
	pos += 2

	propLen, pn, err := decodeV5Varint(frame[pos:end])
	if err != nil {
		return nil, fmt.Errorf("property block length: %w", err)
	}
	pos += pn
	if pos+propLen > end {
		return nil, fmt.Errorf("property block declares %d bytes, %d present", propLen, end-pos)
	}
	// SKIPPED WHOLE. Not walked, not scanned, not looked into.
	pos += propLen

	var filters []string
	for pos < end {
		if pos+2 > end {
			return nil, fmt.Errorf("truncated topic filter length prefix at offset %d", pos)
		}
		filterLen := int(binary.BigEndian.Uint16(frame[pos : pos+2]))
		pos += 2
		if pos+filterLen > end {
			return nil, fmt.Errorf("topic filter declares %d bytes, %d present", filterLen, end-pos)
		}
		filters = append(filters, string(frame[pos:pos+filterLen]))
		pos += filterLen

		// Every filter is followed by exactly one subscription-options byte. A
		// filter without one means the walk has lost its place, so there is
		// nothing honest left to report.
		if pos+1 > end {
			return nil, fmt.Errorf("topic filter at offset %d has no subscription-options byte", pos)
		}
		pos++
	}

	// The walk must land exactly on the frame end. pos > end is unreachable given
	// the checks above, but stating it costs nothing and makes the invariant
	// explicit rather than inferred.
	if pos != end {
		return nil, fmt.Errorf("filter walk ended at %d, want %d", pos, end)
	}

	// MQTT 5.0 3.8.3: "The Payload MUST contain at least one Topic Filter and
	// Subscription Options pair. A SUBSCRIBE packet with no payload is a
	// Protocol Error." Refusing it here is what makes an empty list a framing
	// error rather than an InspectorPacket with no topics for the rules to see.
	if len(filters) == 0 {
		return nil, fmt.Errorf("SUBSCRIBE carries no topic filters")
	}

	return &v5RawSubscribe{PacketID: packetID, Filters: filters}, nil
}

// inspectV5RawSubscribe is inspectV5Subscribe sourced from a HAND-PARSED view
// instead of from the codec, for the frames paho.golang refuses to read. A
// property id outside the codec's table must change nothing about how the packet
// is judged, so this builds the identical InspectorPacket.
//
// WHAT IS MIRRORED, precisely, rather than a bare claim of parity -- the bare
// claim was WRONG for a whole release on the PUBLISH side (68-REVIEW WR-01) and
// a precise list is what makes the next omission visible:
//
//   - SetConnTrack, so Track carries the ORIGINAL client username;
//   - MQTT.Type set to the SAME literal inspectV5Subscribe uses;
//   - MQTT.Topics in FILTER ORDER, the same order the codec path records;
//   - PacketDecider.Decide via the SHARED decideV5Subscribe, so the Allow/Block
//     switch cannot drift between the two paths at all.
//
// Nothing meshtastic is decoded, for the same reason inspectV5Subscribe decodes
// nothing: a SUBSCRIBE has no payload.
//
// THE DELIBERATELY REJECTED ALTERNATIVE is synthesizing a v5.Subscribe and
// filling Raw.MQTT5 with it, so the existing rule branch would match unchanged.
// That is refused because it makes Raw.MQTT5 LIE about where the data came from,
// and the RawPacket doc's never-synthesize invariant exists precisely because a
// synthesized packet is one a rule can mutate without the mutation ever reaching
// the wire -- meshtk#22, shipped once already. The hand-parsed SUBSCRIBE gets its
// own RawPacket member and its own rule branch instead.
func (n *ServerCmd) inspectV5RawSubscribe(socketAddr string, s *v5RawSubscribe) *InspectorPacket {
	ip := &InspectorPacket{
		Log:   n.InspectorLogger,
		Track: &ConnectionInfo{SocketAddress: socketAddr},
		Raw:   &RawPacket{MQTT5RawSub: s},
	}

	// Load-bearing for exactly the reason it is in inspectV5Subscribe: it swaps
	// in the tracked ConnectionInfo carrying the ORIGINAL client username. The
	// CONNECT forwarded to the broker carries the swapped proxy identity, so
	// without this Track.Username is empty and RequireMQTTUserName would Block a
	// subscribe on an already-authenticated session.
	n.SetConnTrack(ip)

	ip.MQTT.Type = "SUBSCRIBE"
	topics := make([]string, 0, len(s.Filters))
	topics = append(topics, s.Filters...)
	ip.MQTT.Topics = topics

	return ip
}

// trackedClientID returns the client id recorded for a socket, or "" when the
// connection has no tracker entry.
//
// It exists so a log line emitted on a path that has NO InspectorPacket -- the
// SUBSCRIBE header-fail branch refuses the frame before an inspector can be
// built -- can still name the client the same way every other production line
// does. Read-only and update-nothing, unlike SetConnTrack and touchConnTrack:
// this is a refusal path, and refreshing a doomed connection's idle timer would
// be a side effect nobody asked for.
//
// The value it returns is CLIENT-CONTROLLED and must go through logSafe at every
// call site.
func (n *ServerCmd) trackedClientID(socketAddr string) string {
	n.ConnMutex.RLock()
	defer n.ConnMutex.RUnlock()
	if info, ok := n.ConnTrack[socketAddr]; ok {
		return info.ClientID
	}
	return ""
}
