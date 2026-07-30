package server

import (
	"encoding/binary"
	"fmt"
)

// v5RawPublish is a hand-parsed view of one MQTT 5.0 PUBLISH frame, produced
// WITHOUT interpreting a single property id.
//
// It exists because paho.golang's Properties.Unpack hard-errors on any property
// id outside its table, and until this file landed a v5.ReadPacket failure made
// handleV5PublishUplink relay the frame untouched -- skipping the topic-alias
// guard, the inspector, PacketDecider.Decide, RewriteHopLimit,
// BlockInvalidEncryption and every Block rule. Three client-chosen bytes in the
// properties block therefore bought a permanent, trivially discoverable
// exemption from the control that exists to stop fleet-wide RF flood
// amplification (CR-04, reproduced in verification as PROBE-A).
//
// Both offsets are kept, not just the payload: spliceV5PublishPayload rebuilds
// the frame from them without having to re-derive anything.
type v5RawPublish struct {
	// QoS comes straight off the fixed-header byte, which is where the packet
	// id's presence is decided.
	QoS byte
	// Topic is REPORTED, never judged. An empty topic is a policy question for
	// the caller (it Blocks); the parser has no business answering it.
	Topic string
	// VarHeaderOffset is the index of the first variable-header byte, i.e. the
	// byte after the remaining-length varint.
	VarHeaderOffset int
	// PayloadOffset is the index of the first payload byte, i.e. the byte after
	// the property block.
	PayloadOffset int
	// Payload aliases frame[PayloadOffset:]; it is not a copy.
	Payload []byte
	// HasTopicAlias reports that the property block carries a Topic Alias
	// property (MQTT 5.0 id 0x23). REPORTED, never judged -- exactly like
	// Topic. handleV5PublishUplink Blocks on it, with the same reason string
	// its codec-parsed sibling uses (68-REVIEW WR-01).
	HasTopicAlias bool
	// AliasScanComplete reports whether the alias walk saw the whole property
	// block. False means the walk gave up early, so HasTopicAlias is a
	// "not found SO FAR" and not a "not present". A false here is a reason to
	// LOG, never a reason to skip inspection -- see scanV5PublishAlias.
	AliasScanComplete bool
	// AliasScanStop is the offset WITHIN the property block at which the walk
	// stopped. It is only meaningful when AliasScanComplete is false, where it
	// names the byte that defeated the walk so ops can extend the modelled id
	// set instead of guessing. It is client-controlled, so it is logged with a
	// numeric verb and never with %v.
	AliasScanStop int
}

// The PUBLISH property ids this walk models, with their MQTT 5.0 §2.2.2 wire
// shapes. This is the COMPLETE set the spec permits on a PUBLISH; any other id
// is either a server-to-client property on the wrong packet type or something
// no version of the spec defines, and either way the walk declines to guess.
const (
	propPayloadFormatIndicator byte = 0x01 // one byte
	propMessageExpiryInterval  byte = 0x02 // four bytes
	propContentType            byte = 0x03 // length-prefixed string
	propResponseTopic          byte = 0x08 // length-prefixed string
	propCorrelationData        byte = 0x09 // length-prefixed binary
	propSubscriptionIdentifier byte = 0x0b // variable byte integer
	propTopicAlias             byte = 0x23 // two bytes
	propUserProperty           byte = 0x26 // two length-prefixed strings
)

// scanV5PublishAlias walks a PUBLISH property block looking for one thing: a
// Topic Alias property. It returns whether an alias was found, whether the walk
// saw the whole block, and where it stopped. It NEVER returns an error, and its
// caller never gets a reason to withhold a parse result because of it.
//
// WHY THIS IS NOT THE CR-04 DEFECT, EVEN THOUGH IT READS PROPERTY IDS. The
// distinction is the whole point of this function and is worth being explicit
// about, because a reader who remembers CR-04 will reasonably flinch at an id
// table appearing in this file.
//
// CR-04 was that an unmodelled property id GATED INSPECTION: paho.golang's
// Properties.Unpack hard-errors on any id outside its table, the error made
// handleV5PublishUplink relay the frame untouched, and three client-chosen bytes
// therefore bought a permanent exemption from the hop clamp, the decider and
// every Block rule. The id table was load-bearing for a decision it had no
// business making.
//
// Here an unmodelled id costs ONE OPTIONAL REFINEMENT and nothing else. The
// frame is still hand-parsed, the topic is still recovered, the ServiceEnvelope
// is still decoded, PacketDecider still runs, RewriteHopLimit still clamps and
// every Block rule still fires. The walk stops, says so
// (action=MQTT5_ALIAS_SCAN_INDETERMINATE), and the packet is judged exactly as
// it would have been without the walk. An id table is acceptable ONLY because of
// that property -- if a future edit ever makes an incomplete walk change a
// decision, the CR-04 defect is back and this comment is the warning.
//
// The residual is bounded and stated: 68-01 strips TopicAliasMaximum from both
// the CONNECT and the CONNACK, so the broker grants a zero alias budget and would
// treat any alias as a protocol error anyway. An alias hidden behind an
// unmodelled id is therefore a frame mosquitto refuses, not a frame that gets
// fanned out unseen.
//
// "complete" means the walk reached a CONCLUSIVE answer, which is either "no
// alias anywhere in this block" (walked to the end) or "an alias is here"
// (found it, stopped). It does not mean "read every byte": once the alias id is
// seen there is nothing left to learn, because the caller Blocks on presence and
// never reads the value.
func scanV5PublishAlias(block []byte) (hasAlias bool, complete bool, stop int) {
	i := 0
	for i < len(block) {
		idAt := i
		id := block[i]
		i++

		switch id {
		case propTopicAlias:
			// Presence is established by the id byte alone. A value that does
			// not fit still means an alias was declared -- so report it, and
			// report the walk as inconclusive, because a truncated property
			// block is not a block anyone can claim to have read.
			if i+2 > len(block) {
				return true, false, idAt
			}
			return true, true, idAt

		case propPayloadFormatIndicator:
			i++
		case propMessageExpiryInterval:
			i += 4

		case propContentType, propResponseTopic, propCorrelationData:
			next, ok := skipV5PropStringOrBinary(block, i)
			if !ok {
				return false, false, idAt
			}
			i = next

		case propUserProperty:
			// A key/value pair -- two length-prefixed strings, and BOTH have to
			// fit or the walk has lost its place.
			next, ok := skipV5PropStringOrBinary(block, i)
			if !ok {
				return false, false, idAt
			}
			next, ok = skipV5PropStringOrBinary(block, next)
			if !ok {
				return false, false, idAt
			}
			i = next

		case propSubscriptionIdentifier:
			_, consumed, err := decodeV5Varint(block[i:])
			if err != nil {
				// A varint that never terminates is a truncated value, handled
				// exactly like every other one: stop, say so, inspect anyway.
				return false, false, idAt
			}
			i += consumed

		default:
			// The one line that matters. An id outside the modelled set costs
			// the refinement and NOTHING ELSE -- no error, no partial claim, and
			// above all no reason for the caller to skip inspection.
			return false, false, idAt
		}

		// A fixed-width value that ran off the end of the block. Same outcome as
		// every other truncation, and checking it here keeps the two fixed-width
		// arms above free of duplicated bounds arithmetic.
		if i > len(block) {
			return false, false, idAt
		}
	}

	return false, true, i
}

// skipV5PropStringOrBinary skips one length-prefixed property value (a UTF-8
// string or binary data -- identical on the wire) starting at pos, returning the
// offset just past it. ok is false when the declared length runs past the block
// end, which is the truncated-value case: not an error, just a walk that cannot
// honestly continue.
func skipV5PropStringOrBinary(block []byte, pos int) (next int, ok bool) {
	if pos+2 > len(block) {
		return 0, false
	}
	n := int(binary.BigEndian.Uint16(block[pos : pos+2]))
	end := pos + 2 + n
	if end > len(block) {
		return 0, false
	}
	return end, true
}

// parseV5PublishFrame reads the topic, the QoS and the payload out of a captured
// MQTT 5.0 PUBLISH frame using only what the wire format guarantees is skippable
// without knowing what any property means:
//
//	byte 0            : 0x3<<4 | DUP<<3 | QoS<<1 | RETAIN
//	varint            : remaining length (1..4 bytes)
//	uint16 + n bytes  : topic name
//	uint16            : packet identifier -- PRESENT ONLY WHEN QoS > 0
//	varint            : property block length in bytes
//	n bytes           : property block -- SKIPPED WHOLE for framing purposes
//	remainder         : payload
//
// Every field before the payload is length-prefixed, which is exactly why the
// payload boundary is computable without a property table. That remains true:
// the block is skipped as a unit, and scanV5PublishAlias' optional walk over the
// same bytes cannot change where the payload starts, whether the frame parses,
// or what this function returns.
//
// It returns an error and NO partial view on any inconsistency: a length prefix
// that does not fit, a remaining length that disagrees with the bytes actually
// present, or a declared length above the packet cap. A frame whose own length
// prefixes contradict its bytes is one mosquitto would refuse too.
func parseV5PublishFrame(frame []byte) (*v5RawPublish, error) {
	if len(frame) < 2 {
		return nil, fmt.Errorf("frame too short: %d bytes", len(frame))
	}
	if frame[0]>>4 != 3 {
		return nil, fmt.Errorf("not a PUBLISH: fixed-header type %d", frame[0]>>4)
	}

	qos := (frame[0] >> 1) & 0x03
	if qos == 3 {
		// Malformed per MQTT 5.0 3.3.1.2; the packet-id layout is undefined for
		// it, so there is nothing honest to parse.
		return nil, fmt.Errorf("malformed PUBLISH: QoS 3")
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

	varHeaderOffset := 1 + n
	end := varHeaderOffset + remLen
	if end != len(frame) {
		return nil, fmt.Errorf("remaining length %d disagrees with the %d bytes present", remLen, len(frame)-varHeaderOffset)
	}

	pos := varHeaderOffset

	if pos+2 > end {
		return nil, fmt.Errorf("truncated topic length prefix")
	}
	topicLen := int(binary.BigEndian.Uint16(frame[pos : pos+2]))
	pos += 2
	if pos+topicLen > end {
		return nil, fmt.Errorf("topic declares %d bytes, %d present", topicLen, end-pos)
	}
	topic := string(frame[pos : pos+topicLen])
	pos += topicLen

	if qos > 0 {
		if pos+2 > end {
			return nil, fmt.Errorf("truncated packet identifier")
		}
		pos += 2
	}

	propLen, pn, err := decodeV5Varint(frame[pos:end])
	if err != nil {
		return nil, fmt.Errorf("property block length: %w", err)
	}
	pos += pn
	if pos+propLen > end {
		return nil, fmt.Errorf("property block declares %d bytes, %d present", propLen, end-pos)
	}
	// The property block is skipped WHOLE for framing: the payload boundary is
	// computed from the declared length and never from what the properties mean.
	// The alias walk below reads the SAME bytes purely to refine what gets
	// REPORTED, and it cannot fail this parse -- it returns no error, and both of
	// its answers are recorded rather than acted on here. See scanV5PublishAlias
	// for why that separation is what keeps CR-04 closed.
	hasAlias, scanComplete, scanStop := scanV5PublishAlias(frame[pos : pos+propLen])
	pos += propLen

	return &v5RawPublish{
		QoS:               qos,
		Topic:             topic,
		VarHeaderOffset:   varHeaderOffset,
		PayloadOffset:     pos,
		Payload:           frame[pos:end],
		HasTopicAlias:     hasAlias,
		AliasScanComplete: scanComplete,
		AliasScanStop:     scanStop,
	}, nil
}

// spliceV5PublishPayload rebuilds a captured PUBLISH frame around a new payload:
// the original fixed-header byte, a re-encoded remaining length, the original
// variable-header bytes copied VERBATIM, then the new payload.
//
// This is strictly stronger than a codec round trip on this path, and that is
// the whole reason it exists. The codec cannot represent the properties it
// refused to parse, so re-encoding would either fail or silently drop them;
// copying frame[VarHeaderOffset:PayloadOffset] preserves every byte exactly --
// unmodelled property ids included. The payload is the only field the proxy ever
// changes on an uplink PUBLISH (the hop clamp and the payload censor both end in
// setPublishPayload), so nothing else needs re-encoding.
func spliceV5PublishPayload(frame []byte, p *v5RawPublish, newPayload []byte) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("cannot splice: no parsed view")
	}
	if p.VarHeaderOffset < 1 || p.PayloadOffset < p.VarHeaderOffset || p.PayloadOffset > len(frame) {
		return nil, fmt.Errorf("cannot splice: offsets %d/%d out of range for a %d byte frame",
			p.VarHeaderOffset, p.PayloadOffset, len(frame))
	}

	varHeader := frame[p.VarHeaderOffset:p.PayloadOffset]
	remLen := len(varHeader) + len(newPayload)
	if remLen > maxV5PacketBytes {
		return nil, fmt.Errorf("v5 packet too large after rewrite: %d bytes", remLen)
	}

	lenBytes := encodeV5Varint(remLen)
	out := make([]byte, 0, 1+len(lenBytes)+remLen)
	out = append(out, frame[0])
	out = append(out, lenBytes...)
	out = append(out, varHeader...)
	out = append(out, newPayload...)
	return out, nil
}

// decodeV5Varint decodes an MQTT variable byte integer, mirroring readFrame's
// loop including its 4-byte termination guard. It returns the value and how many
// bytes it consumed.
func decodeV5Varint(b []byte) (value int, consumed int, err error) {
	var mult uint32
	for i := 0; ; i++ {
		if i == 4 {
			return 0, 0, fmt.Errorf("malformed variable byte integer")
		}
		if i >= len(b) {
			return 0, 0, fmt.Errorf("truncated variable byte integer")
		}
		d := b[i]
		value |= int(d&0x7F) << mult
		if d&0x80 == 0 {
			return value, i + 1, nil
		}
		mult += 7
	}
}

// encodeV5Varint is the inverse of decodeV5Varint. Callers must cap the value
// first; maxV5PacketBytes always fits in three bytes.
func encodeV5Varint(v int) []byte {
	var out []byte
	for {
		d := byte(v % 128)
		v /= 128
		if v > 0 {
			d |= 0x80
		}
		out = append(out, d)
		if v == 0 {
			return out
		}
	}
}
