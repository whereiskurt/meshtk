package server

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/whereiskurt/meshtk/pkg/config"
)

// mockAuthenticator implements Authenticator for testing.
type mockAuthenticator struct {
	valid     bool
	err       error
	callCount int
}

func (m *mockAuthenticator) Verify(_ context.Context, _ string, _ []byte) (bool, error) {
	m.callCount++
	return m.valid, m.err
}

// newTestServerCmd creates a minimal ServerCmd with the given mock authenticator.
func newTestServerCmd(auth *mockAuthenticator) *ServerCmd {
	logger := log.New()
	logger.SetLevel(log.WarnLevel)

	n := &ServerCmd{
		Config: &config.Config{},
		InspectorLogger: logger,
	}
	n.Config.Server.ProxyUsername = "proxy"
	n.Config.Server.ProxyPassword = "proxypass"
	n.Config.Server.CredCache.Passthrough = []string{"ghosts"}
	n.Config.Server.CredCache.TimeoutSecs = 5
	n.ConnTrack = make(map[string]*ConnectionInfo)
	n.Authenticator = auth

	return n
}

func TestInspectAuth_ValidCredentials_SwapsToGeneric(t *testing.T) {
	mock := &mockAuthenticator{valid: true}
	n := newTestServerCmd(mock)

	connectPacket := &packets.ConnectPacket{
		FixedHeader:      packets.FixedHeader{MessageType: packets.Connect},
		Username:         "validuser",
		Password:         []byte("validpass"),
		ClientIdentifier: "test-client",
	}
	var cp packets.ControlPacket = connectPacket

	clientConn, readConn := net.Pipe()
	defer readConn.Close()
	defer clientConn.Close()

	ip := &InspectorPacket{
		Log:   log.New(),
		Track: &ConnectionInfo{SocketAddress: "127.0.0.1:12345"},
		Raw:   &RawPacket{MQTT: &cp},
	}

	ip.inspectRawPacket(n, clientConn)

	// Verify credentials were swapped
	p := (*ip.Raw.MQTT).(*packets.ConnectPacket)
	assert.Equal(t, "proxy", p.Username)
	assert.Equal(t, []byte("proxypass"), p.Password)
	assert.False(t, ip.AuthRejected)
	assert.Equal(t, 1, mock.callCount, "Verify should be called once")
}

func TestInspectAuth_InvalidCredentials_RejectsWithConnack(t *testing.T) {
	mock := &mockAuthenticator{valid: false}
	n := newTestServerCmd(mock)

	connectPacket := &packets.ConnectPacket{
		FixedHeader:      packets.FixedHeader{MessageType: packets.Connect},
		Username:         "baduser",
		Password:         []byte("badpass"),
		ClientIdentifier: "test-client",
	}
	var cp packets.ControlPacket = connectPacket

	clientConn, readConn := net.Pipe()

	ip := &InspectorPacket{
		Log:   log.New(),
		Track: &ConnectionInfo{SocketAddress: "127.0.0.1:12345"},
		Raw:   &RawPacket{MQTT: &cp},
	}

	// Run inspectRawPacket in background since pipe write may block
	done := make(chan struct{})
	go func() {
		ip.inspectRawPacket(n, clientConn)
		close(done)
	}()

	// Read CONNACK from the other end of the pipe
	readConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	respPacket, err := packets.ReadPacket(readConn)
	require.NoError(t, err)
	readConn.Close()
	clientConn.Close()

	connack, ok := respPacket.(*packets.ConnackPacket)
	require.True(t, ok, "expected ConnackPacket")
	assert.Equal(t, byte(packets.ErrRefusedNotAuthorised), connack.ReturnCode)

	<-done
	assert.True(t, ip.AuthRejected)
	assert.Equal(t, 1, mock.callCount)
}

func TestInspectAuth_EmptyUsername_RejectsWithConnack(t *testing.T) {
	mock := &mockAuthenticator{valid: false}
	n := newTestServerCmd(mock)

	connectPacket := &packets.ConnectPacket{
		FixedHeader:      packets.FixedHeader{MessageType: packets.Connect},
		Username:         "",
		Password:         []byte(""),
		ClientIdentifier: "test-client",
	}
	var cp packets.ControlPacket = connectPacket

	clientConn, readConn := net.Pipe()

	ip := &InspectorPacket{
		Log:   log.New(),
		Track: &ConnectionInfo{SocketAddress: "127.0.0.1:12345"},
		Raw:   &RawPacket{MQTT: &cp},
	}

	done := make(chan struct{})
	go func() {
		ip.inspectRawPacket(n, clientConn)
		close(done)
	}()

	readConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	respPacket, err := packets.ReadPacket(readConn)
	require.NoError(t, err)
	readConn.Close()
	clientConn.Close()

	connack, ok := respPacket.(*packets.ConnackPacket)
	require.True(t, ok, "expected ConnackPacket")
	assert.Equal(t, byte(packets.ErrRefusedNotAuthorised), connack.ReturnCode)

	<-done
	assert.True(t, ip.AuthRejected)
	assert.Equal(t, 0, mock.callCount, "Verify should NOT be called for empty username")
}

func TestInspectAuth_PassthroughUsername_BypassesAuth(t *testing.T) {
	mock := &mockAuthenticator{valid: false}
	n := newTestServerCmd(mock)

	connectPacket := &packets.ConnectPacket{
		FixedHeader:      packets.FixedHeader{MessageType: packets.Connect},
		Username:         "ghosts",
		Password:         []byte("ghostpass"),
		ClientIdentifier: "test-client",
	}
	var cp packets.ControlPacket = connectPacket

	clientConn, readConn := net.Pipe()
	defer readConn.Close()
	defer clientConn.Close()

	ip := &InspectorPacket{
		Log:   log.New(),
		Track: &ConnectionInfo{SocketAddress: "127.0.0.1:12345"},
		Raw:   &RawPacket{MQTT: &cp},
	}

	ip.inspectRawPacket(n, clientConn)

	// Passthrough: credentials should NOT be swapped
	p := (*ip.Raw.MQTT).(*packets.ConnectPacket)
	assert.Equal(t, "ghosts", p.Username)
	assert.Equal(t, []byte("ghostpass"), p.Password)
	assert.False(t, ip.AuthRejected)
	assert.Equal(t, 0, mock.callCount, "Verify should NOT be called for passthrough")
}

func TestInspectAuth_VerifyError_RejectsWithConnack(t *testing.T) {
	mock := &mockAuthenticator{valid: false, err: fmt.Errorf("dynamodb timeout")}
	n := newTestServerCmd(mock)

	connectPacket := &packets.ConnectPacket{
		FixedHeader:      packets.FixedHeader{MessageType: packets.Connect},
		Username:         "erroruser",
		Password:         []byte("errorpass"),
		ClientIdentifier: "test-client",
	}
	var cp packets.ControlPacket = connectPacket

	clientConn, readConn := net.Pipe()

	ip := &InspectorPacket{
		Log:   log.New(),
		Track: &ConnectionInfo{SocketAddress: "127.0.0.1:12345"},
		Raw:   &RawPacket{MQTT: &cp},
	}

	done := make(chan struct{})
	go func() {
		ip.inspectRawPacket(n, clientConn)
		close(done)
	}()

	readConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	respPacket, err := packets.ReadPacket(readConn)
	require.NoError(t, err)
	readConn.Close()
	clientConn.Close()

	connack, ok := respPacket.(*packets.ConnackPacket)
	require.True(t, ok, "expected ConnackPacket")
	assert.Equal(t, byte(packets.ErrRefusedNotAuthorised), connack.ReturnCode)

	<-done
	assert.True(t, ip.AuthRejected)
	assert.Equal(t, 1, mock.callCount)
}
