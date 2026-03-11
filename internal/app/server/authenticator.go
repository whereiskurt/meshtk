package server

import (
	"context"
	"net"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

// Authenticator validates MQTT CONNECT credentials.
// Implementations handle cache lookup, backend fetch, and password verification.
type Authenticator interface {
	// Verify checks if the given username/password combination is valid.
	// password is the raw bytes from the MQTT CONNECT packet.
	// Returns (true, nil) on valid credentials, (false, nil) on invalid,
	// or (false, err) on backend failure.
	Verify(ctx context.Context, username string, password []byte) (bool, error)
}

// writeConnackRejection writes an MQTT CONNACK packet with return code 0x05
// (Not Authorised) to the given connection.
func writeConnackRejection(conn net.Conn) error {
	connack := &packets.ConnackPacket{
		FixedHeader: packets.FixedHeader{MessageType: packets.Connack},
		ReturnCode:  packets.ErrRefusedNotAuthorised,
	}
	return connack.Write(conn)
}
