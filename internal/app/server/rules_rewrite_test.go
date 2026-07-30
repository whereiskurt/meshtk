package server

import (
	"bytes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"reflect"
	"runtime/debug"
	"testing"

	"github.com/eclipse/paho.mqtt.golang/packets"
	log "github.com/sirupsen/logrus"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// These tests pin the two shared-chain defects closed by 69-01, and they are
// deliberately CROSS-CODEC: both live in helpers reached from the 3.1.1 loop and
// the v5 loop alike, so a fix proven on one codec proves nothing about the other.
//
//  1. CR-01 -- RewritePayloadString dereferenced Meshtastic.Cipher, which is
//     assigned ONLY in inspectMeshtastic's decrypt branch. A DECODED
//     (unencrypted) TEXT_MESSAGE_APP therefore reached it with a nil cipher and
//     SIGSEGV'd. There is no recover() on the proxy read loop, so one
//     authenticated plaintext text message killed the whole process and dropped
//     every connected radio. Two existing v5 test helpers picked NODEINFO
//     fixtures specifically to route AROUND this crash; nothing asserted the
//     non-crash until this file.
//
//  2. CR-03 -- the rewrite rebuilt meshtastic.Data from three fields, so
//     proto.Marshal emitted only those three and want_response, dest, source,
//     request_id, reply_id and emoji were re-encrypted away. Because the rewrite
//     CALL was never gated on the username (only the word replacement was), that
//     was every text message on the fleet: 2.8 tapbacks, threaded replies,
//     delivery-ACK requests and DM routing fields all vanished between the sender
//     and the mesh.
//
// Every preservation assertion below is made on a DECRYPT ROUND TRIP off the
// FORWARDED bytes, never on the parsed struct -- a struct read would pass even
// if the rewrite never reached the wire (meshtk#22).

// A real 16-byte AES channel key. The rewrite path cannot be exercised at all
// without one: no cipher, no re-encrypt, no test.
const rewriteTestKey = "1PG7OiApB1nwvP+rz05pAQ=="

const (
	rewriteTopic = "msh/US/2/e/dc.run/!435990e4"
	rewriteGw    = "!435990e4"
	rewriteFrom  = uint32(0x435990e4)
	rewriteID    = uint32(0x1234abcd)

	// The plaintext the sender wrote, and what the "public" censor turns it into
	// ("hi" -> "bye"). Using a payload the censor ACTUALLY rewrites is what makes
	// "only the payload changed" a falsifiable claim rather than a tautology.
	rewritePlain    = "hi there"
	rewriteCensored = "bye there"

	// Distinguishable non-zero values for the six fields CR-03 dropped, so a
	// failure names the field that regressed instead of reporting "not equal".
	wantDest      = uint32(0x11112222)
	wantSource    = uint32(0x33334444)
	wantRequestID = uint32(0x55556666)
	wantReplyID   = uint32(0x77778888)
	wantEmoji     = uint32(1)
	wantBitfield  = uint32(1)
)

// addTestChannel registers a channel + cipher on a test ServerCmd so
// inspectMeshtastic takes its DECRYPT branch and populates Meshtastic.Cipher.
//
// Reflection, because config.Meshtastic.Channels is a slice of an ANONYMOUS
// struct: a composite literal would have to repeat the field tags byte for byte
// and would silently stop compiling (or worse, need re-copying) the next time a
// tag moves. DecryptMeshtastic indexes Channels in lockstep with Ciphers, so
// both must grow together.
func addTestChannel(t *testing.T, n *ServerCmd, base64Key string) {
	t.Helper()
	channels := reflect.ValueOf(&n.Config.Meshtastic.Channels).Elem()
	ch := reflect.New(channels.Type().Elem()).Elem()
	ch.FieldByName("Slot").SetString("primary")
	ch.FieldByName("Name").SetString("LongFast")
	ch.FieldByName("EncryptKey").SetString(base64Key)
	ch.FieldByName("IsEncrypted").SetBool(true)
	ch.FieldByName("IsPrimary").SetBool(true)
	channels.Set(reflect.Append(channels, ch))

	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		t.Fatalf("decode channel key: %v", err)
	}
	n.Ciphers = append(n.Ciphers, NewAESCipher(key))
}

// fullFieldData is a TEXT_MESSAGE_APP Data with every field CR-03 dropped set to
// a distinguishable non-zero value.
func fullFieldData() *meshtastic.Data {
	return &meshtastic.Data{
		Portnum:      meshtastic.PortNum_TEXT_MESSAGE_APP,
		Payload:      []byte(rewritePlain),
		WantResponse: true,
		Dest:         wantDest,
		Source:       wantSource,
		RequestId:    wantRequestID,
		ReplyId:      wantReplyID,
		Emoji:        wantEmoji,
		Bitfield:     proto.Uint32(wantBitfield),
	}
}

// encryptData applies the same CTR construction DecryptMeshtastic reverses:
// a 16-byte nonce carrying the packet id at [0:] and the sender at [8:].
func encryptData(t *testing.T, block cipher.Block, id, from uint32, d *meshtastic.Data) []byte {
	t.Helper()
	raw, err := proto.Marshal(d)
	if err != nil {
		t.Fatalf("marshal Data: %v", err)
	}
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], id)
	binary.LittleEndian.PutUint32(nonce[8:], from)
	out := make([]byte, len(raw))
	cipher.NewCTR(block, nonce).XORKeyStream(out, raw)
	return out
}

// encryptedTextEnvelope is what a real radio uplinks: a six-field
// TEXT_MESSAGE_APP encrypted under the channel key. Hop budget is deliberately
// sane (3/3) so RewriteHopLimit stays out of the way and the only rewrite under
// test is the payload censor.
func encryptedTextEnvelope(t *testing.T, block cipher.Block) *meshtastic.ServiceEnvelope {
	t.Helper()
	return &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From: rewriteFrom, To: 0xffffffff, Id: rewriteID, HopLimit: 3, HopStart: 3,
			PayloadVariant: &meshtastic.MeshPacket_Encrypted{
				Encrypted: encryptData(t, block, rewriteID, rewriteFrom, fullFieldData()),
			},
		},
		GatewayId: rewriteGw,
		ChannelId: "dc.run",
	}
}

// decodedTextEnvelope is the packet that used to kill the process: a
// TEXT_MESSAGE_APP that arrived with a DECODED payload, so nothing ever assigns
// Meshtastic.Cipher.
func decodedTextEnvelope() *meshtastic.ServiceEnvelope {
	return &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From: rewriteFrom, To: 0xffffffff, Id: rewriteID, HopLimit: 3, HopStart: 3,
			PayloadVariant: &meshtastic.MeshPacket_Decoded{Decoded: fullFieldData()},
		},
		GatewayId: rewriteGw,
		ChannelId: "dc.run",
	}
}

// newRewriteTestPacket wraps an envelope in a 3.1.1 PUBLISH and runs the real
// inspectMeshtastic over it, mirroring newHopTestPacket but going through the
// inspector so Cipher/WasEncrypted are populated exactly as they are in prod.
func newRewriteTestPacket(t *testing.T, n *ServerCmd, username string, env *meshtastic.ServiceEnvelope) (*InspectorPacket, *packets.PublishPacket) {
	t.Helper()
	payload, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	pub := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	pub.TopicName = rewriteTopic
	pub.Payload = payload

	var cp packets.ControlPacket = pub
	logger := log.New()
	logger.SetLevel(log.PanicLevel)
	ip := &InspectorPacket{
		Log:   logger,
		Track: &ConnectionInfo{Username: username, SocketAddress: v5PubAddr},
		Raw:   &RawPacket{MQTT: &cp, Meshtastic: env},
	}
	ip.MQTT.Type = "PUBLISH"
	ip.MQTT.Topics = []string{rewriteTopic}
	ip.inspectMeshtastic(n)
	return ip, pub
}

func helloGoodbyeRule(t *testing.T) Rule {
	t.Helper()
	for _, r := range rewriteRules() {
		if r.Name == "RewriteHelloGoodbye" {
			return r
		}
	}
	t.Fatal("RewriteHelloGoodbye rule not found")
	return Rule{}
}

// mustNotPanic turns the CR-01 SIGSEGV into a named test failure instead of a
// crashed test binary, so a regression reports WHICH call died and where.
func mustNotPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s PANICKED: %v\n"+
				"This is CR-01: RewritePayloadString dereferencing a nil channel cipher.\n"+
				"It is not one dropped connection -- there is no recover() on the proxy\n"+
				"read loop, so this kills the process and every connected radio.\n%s",
				what, r, debug.Stack())
		}
	}()
	fn()
}

// assertSixFields is the ONE six-field comparison, called by both preservation
// tests so the 3.1.1 and v5 assertions cannot drift apart and a regression names
// the same field whichever codec dropped it.
func assertSixFields(t *testing.T, codec string, got *meshtastic.Data) {
	t.Helper()

	if !got.GetWantResponse() {
		t.Errorf("[%s] want_response LOST across the rewrite (delivery ACKs stop working)", codec)
	}
	if got.GetDest() != wantDest {
		t.Errorf("[%s] dest = %#08x, want %#08x (DM routing field lost)", codec, got.GetDest(), wantDest)
	}
	if got.GetSource() != wantSource {
		t.Errorf("[%s] source = %#08x, want %#08x (DM routing field lost)", codec, got.GetSource(), wantSource)
	}
	if got.GetRequestId() != wantRequestID {
		t.Errorf("[%s] request_id = %#08x, want %#08x (routing/response correlation lost)", codec, got.GetRequestId(), wantRequestID)
	}
	if got.GetReplyId() != wantReplyID {
		t.Errorf("[%s] reply_id = %#08x, want %#08x (2.8 threaded replies lost)", codec, got.GetReplyId(), wantReplyID)
	}
	if got.GetEmoji() != wantEmoji {
		t.Errorf("[%s] emoji = %d, want %d (2.8 tapbacks lost)", codec, got.GetEmoji(), wantEmoji)
	}

	// Not part of the six, but the rewrite must not regress them either: Portnum
	// keeps the message routable and an absent Bitfield makes 2.8 radios drop the
	// packet outright (meshtk#21 pre-hop drop).
	if got.GetPortnum() != meshtastic.PortNum_TEXT_MESSAGE_APP {
		t.Errorf("[%s] portnum = %s, want TEXT_MESSAGE_APP", codec, got.GetPortnum())
	}
	if got.Bitfield == nil {
		t.Errorf("[%s] bitfield absent; 2.8 radios drop the packet (pre-hop drop)", codec)
	} else if got.GetBitfield() != wantBitfield {
		t.Errorf("[%s] bitfield = %d, want %d", codec, got.GetBitfield(), wantBitfield)
	}

	// ...and the payload is the ONE field that is supposed to have moved.
	if string(got.GetPayload()) != rewriteCensored {
		t.Errorf("[%s] payload = %q, want %q (the censored text never reached the wire)", codec, got.GetPayload(), rewriteCensored)
	}
}

// decryptWireEnvelope decrypts the re-encrypted Data back off a forwarded
// ServiceEnvelope, which is the only way to see what the mesh will actually
// receive.
func decryptWireEnvelope(t *testing.T, n *ServerCmd, env *meshtastic.ServiceEnvelope) *meshtastic.Data {
	t.Helper()
	encrypted := env.GetPacket().GetEncrypted()
	if len(encrypted) == 0 {
		t.Fatal("forwarded packet carries no encrypted payload; the rewrite did not re-encrypt")
	}
	got, _, _, err := n.DecryptMeshtastic(env.GetPacket().GetId(), env.GetPacket().GetFrom(), encrypted)
	if err != nil {
		t.Fatalf("decrypt forwarded payload: %v", err)
	}
	return got
}

// --- CR-01: the nil cipher --------------------------------------------------

// A decoded packet has no channel cipher, so the rewrite is impossible. It must
// SAY so and change nothing -- not dereference nil.
func TestRewritePayloadStringNilCipherReturnsError(t *testing.T) {
	n := newTestServerCmd(&mockAuthenticator{valid: true})
	addTestChannel(t, n, rewriteTestKey)

	ip, pub := newRewriteTestPacket(t, n, "public", decodedTextEnvelope())
	if ip.Meshtastic.Cipher != nil {
		t.Fatal("fixture is wrong: a DECODED packet must leave Meshtastic.Cipher nil")
	}
	before := append([]byte(nil), pub.Payload...)

	var err error
	mustNotPanic(t, "RewritePayloadString on a nil-cipher packet", func() {
		err = ip.RewritePayloadString()
	})

	if err == nil {
		t.Fatal("RewritePayloadString returned nil for a packet it cannot re-encrypt")
	}
	if !bytes.Equal(pub.Payload, before) {
		t.Errorf("the PUBLISH payload was mutated by a rewrite that failed:\n before %x\n after  %x", before, pub.Payload)
	}
	if ip.WireRewritten {
		t.Error("WireRewritten set by a failed rewrite; the forwarder would re-encode a packet nothing changed")
	}
}

// --- CR-03: the six dropped fields ------------------------------------------

// The shared helper: every Data field must survive a decrypt round trip off the
// forwarded bytes, with only the payload changed.
func TestRewritePayloadStringPreservesDataFields(t *testing.T) {
	n := newTestServerCmd(&mockAuthenticator{valid: true})
	addTestChannel(t, n, rewriteTestKey)

	ip, pub := newRewriteTestPacket(t, n, "public", encryptedTextEnvelope(t, n.Ciphers[0]))
	if ip.Meshtastic.Cipher == nil {
		t.Fatal("fixture is wrong: inspectMeshtastic never took the decrypt branch")
	}
	if ip.Meshtastic.PayloadString != rewritePlain {
		t.Fatalf("decoded payload = %q, want %q", ip.Meshtastic.PayloadString, rewritePlain)
	}

	// What the censor does to it before the re-encrypt.
	ip.Meshtastic.PayloadString = rewriteCensored

	if err := ip.RewritePayloadString(); err != nil {
		t.Fatalf("rewrite failed on a packet with a real cipher: %v", err)
	}
	if !ip.WireRewritten {
		t.Fatal("WireRewritten not set; the forwarder would relay the ORIGINAL frame (meshtk#22)")
	}

	env := &meshtastic.ServiceEnvelope{}
	if err := proto.Unmarshal(pub.Payload, env); err != nil {
		t.Fatalf("unmarshal forwarded payload: %v", err)
	}
	assertSixFields(t, "3.1.1", decryptWireEnvelope(t, n, env))
}

// The SAME six-field assertion, driven through the REAL v5 uplink handler.
// ROADMAP Success Criterion 2 requires field preservation "on both codecs": the
// helper-level test above proves the helper, and this one proves the v5 dispatch
// actually reaches it with a populated cipher.
func TestDataFieldsSurviveRewriteOnV5Uplink(t *testing.T) {
	n, _ := v5PublishServer(t, "public")
	addTestChannel(t, n, rewriteTestKey)

	frame := v5PublishFrame(t, encryptedTextEnvelope(t, n.Ciphers[0]))

	var backend bytes.Buffer
	ok := false
	mustNotPanic(t, "handleV5PublishUplink on an encrypted TEXT_MESSAGE", func() {
		ok = n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame)
	})
	if !ok {
		t.Fatal("connection dropped for a packet the rules allow")
	}

	out := backend.Bytes()
	if bytes.Equal(out, frame) {
		t.Fatal("the captured frame was forwarded verbatim; the v5 dispatch never reached RewritePayloadString")
	}

	_, env := wireEnvelope(t, out)
	assertSixFields(t, "v5", decryptWireEnvelope(t, n, env))
}

// --- CR-01 layer 2: the matcher declines ------------------------------------

// The rewrite must not be ENTERED for a packet it cannot perform, on either
// codec. This is the guard that keeps a plaintext text message away from the
// crash site even if the helper's own guard were ever removed.
func TestRewriteHelloGoodbyeDeclinesDecodedTextMessage(t *testing.T) {
	rule := helloGoodbyeRule(t)

	t.Run("3.1.1", func(t *testing.T) {
		n := newTestServerCmd(&mockAuthenticator{valid: true})
		addTestChannel(t, n, rewriteTestKey)

		ip, pub := newRewriteTestPacket(t, n, "public", decodedTextEnvelope())
		before := append([]byte(nil), pub.Payload...)

		matched := true
		mustNotPanic(t, "RewriteHelloGoodbye matcher on a decoded 3.1.1 TEXT_MESSAGE", func() {
			matched = rule.Matcher(ip)
		})
		if matched {
			t.Error("matcher entered a rewrite it cannot perform (no channel cipher)")
		}
		if !bytes.Equal(pub.Payload, before) {
			t.Error("the PUBLISH payload was mutated by a declined rewrite")
		}
		if ip.WireRewritten {
			t.Error("WireRewritten set by a declined rewrite")
		}
	})

	t.Run("v5", func(t *testing.T) {
		n, _ := v5PublishServer(t, "public")
		addTestChannel(t, n, rewriteTestKey)

		cp := v5PublishPacket(t, decodedTextEnvelope())
		ip := n.inspectV5Publish(v5PubAddr, cp)
		if ip.Meshtastic.Cipher != nil {
			t.Fatal("fixture is wrong: a DECODED packet must leave Meshtastic.Cipher nil")
		}

		matched := true
		mustNotPanic(t, "RewriteHelloGoodbye matcher on a decoded v5 TEXT_MESSAGE", func() {
			matched = rule.Matcher(ip)
		})
		if matched {
			t.Error("matcher entered a rewrite it cannot perform (no channel cipher)")
		}
		if ip.WireRewritten {
			t.Error("WireRewritten set by a declined rewrite")
		}
	})
}

// End to end on both codecs: a decoded TEXT_MESSAGE_APP is JUDGED without
// killing the process. This is the assertion 68-REVIEW WR-13 item 5 said was
// missing -- two v5 helpers picked NODEINFO fixtures specifically to avoid this
// path, so the crash shipped uncovered.
func TestDecodedTextMessageSurvivesBothCodecs(t *testing.T) {
	t.Run("3.1.1", func(t *testing.T) {
		n := newTestServerCmd(&mockAuthenticator{valid: true})
		n.LoadInspectorRules()
		addTestChannel(t, n, rewriteTestKey)

		ip, _ := newRewriteTestPacket(t, n, "public", decodedTextEnvelope())

		var result DecisionResult
		mustNotPanic(t, "PacketDecider.Decide on a decoded 3.1.1 TEXT_MESSAGE", func() {
			result = n.PacketDecider.Decide(ip)
		})
		if result.Decision == Block {
			t.Fatalf("decider Blocked an authenticated plaintext text message: %s", result.Reason)
		}
		if result.Decision != Allow {
			t.Errorf("decision = %v, want Allow (AllowedMeshtasticApps covers TEXT_MESSAGE_APP)", result.Decision)
		}
	})

	t.Run("v5", func(t *testing.T) {
		n, _ := v5PublishServer(t, "public")
		addTestChannel(t, n, rewriteTestKey)

		frame := v5PublishFrame(t, decodedTextEnvelope())

		var backend bytes.Buffer
		ok := false
		mustNotPanic(t, "handleV5PublishUplink on a decoded TEXT_MESSAGE", func() {
			ok = n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame)
		})
		if !ok {
			t.Fatal("connection dropped for a decoded text message the rules allow")
		}
		// Nothing could rewrite it, so the captured frame must forward untouched.
		if !bytes.Equal(backend.Bytes(), frame) {
			t.Fatalf("forwarded bytes drifted for a packet no rule rewrote:\n in  %x\n out %x", frame, backend.Bytes())
		}
	})
}
