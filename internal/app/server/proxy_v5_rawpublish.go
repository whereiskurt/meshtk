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
//	n bytes           : property block -- SKIPPED WHOLE, never interpreted
//	remainder         : payload
//
// Every field before the payload is length-prefixed, which is exactly why the
// payload boundary is computable without a property table.
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
	// The property block is skipped WHOLE. Reading one id here would put a
	// property table back in the inspection path, which is the defect.
	pos += propLen

	return &v5RawPublish{
		QoS:             qos,
		Topic:           topic,
		VarHeaderOffset: varHeaderOffset,
		PayloadOffset:   pos,
		Payload:         frame[pos:end],
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
