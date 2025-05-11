package server

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
	"github.com/spf13/cobra"
	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/pkg/config"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

type ServerCmd struct {
	Config    *config.Config
	CmdOutput struct {
		WasSuccess bool
	}

	FrontendPool *ConnectionPool
	BackendPool  *BackendConnectionPool
	ConnTrack    map[string]*ConnectionInfo // maps connection ID to client ID
	ConnMutex    sync.RWMutex

	Ciphers       []cipher.Block
	PacketDecider Decider // Interface for making packet routing decisions
	LogFileMutex  sync.RWMutex
}

// NewBackendConnectionPool creates a new backend connection pool
func (n *ServerCmd) NewBackendConnectionPool(address string, maxSize int, dialTimeout time.Duration) {
	r := &BackendConnectionPool{
		pool:        make([]*BackendConn, 0, maxSize),
		address:     address,
		maxSize:     maxSize,
		currentSize: 0,
		dialTimeout: dialTimeout,
	}
	n.BackendPool = r

	preConnectCount := maxSize / 4
	successCount := 0
	n.Config.Log.Infof("Pre-establishing %d backend connections to %s", preConnectCount, address)

	for i := range preConnectCount {
		backendConn, err := n.BackendPool.Get()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			n.Config.Log.Warnf("Failed to pre-establish backend connection #%d: %v", i+1, err)
			continue
		}
		n.BackendPool.Put(backendConn)
		successCount++

		if (i+1)%10 == 0 {
			n.Config.Log.Infof("Pre-established %d/%d backend connections", i+1, preConnectCount)
		}
	}
	n.Config.Log.Infof("✅ Successfully pre-established %d/%d backend connections", successCount, preConnectCount)
}

func NewAESCipher(key []byte) cipher.Block {
	c, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	return c
}

func NewServer(c *config.Config) (n *ServerCmd) {
	n = new(ServerCmd)
	n.Config = c
	n.SetupTracker()
	n.LoadCiphers(c)
	n.LoadInspectorRules()

	return n
}

func (n *ServerCmd) LoadCiphers(c *config.Config) {
	for _, channel := range n.Config.Meshtastic.Channels {
		base64Key := channel.EncryptKey
		keyBytes, err := base64.StdEncoding.DecodeString(base64Key)
		if err != nil {
			c.Log.Fatalf("The %s channel key '%s' is invalid hex: %+v", channel.Name, base64Key, err)
		}
		// Expand the single byte key to 16 bytes for AES-256
		if len(keyBytes) == 1 && base64Key == "AQ==" {
			keyBytes = append(keyBytes, make([]byte, 15)...)
		}
		n.Ciphers = append(n.Ciphers, NewAESCipher(keyBytes))
	}
}

func (n *ServerCmd) DecryptMeshtastic(id, from uint32, payload []byte) (decoded *meshtastic.Data, hexkey string, c *cipher.Block, err error) {
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], id)
	binary.LittleEndian.PutUint32(nonce[8:], from)
	decrypted := make([]byte, len(payload))

	for k, cipherInstance := range n.Ciphers {
		hexKey := n.Config.Meshtastic.Channels[k].EncryptKey

		cipher.NewCTR(cipherInstance, nonce).XORKeyStream(decrypted, payload)
		decoded = new(meshtastic.Data)
		if err := proto.Unmarshal(decrypted, decoded); err == nil {
			return decoded, hexKey, &cipherInstance, nil
		}

	}
	return nil, "", nil, fmt.Errorf("failed to decrypt data with any cipher")
}

func (n *ServerCmd) Help(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	fmt.Fprintln(n.Config.Stdout, help.ServerHelp(n.Config))
}

func (n *ServerCmd) ProtobufServer(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("protobuf.InspectorServer")
	n.Config.Log.Tracef("%+v", n.Config)

	n.StartProtobufServer()
}

func (n *ServerCmd) ProxyServer(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("protobuf.ProxyServer")
	n.Config.Log.Tracef("%+v", n.Config)

	n.StartProxyServer()
}

func (n *ServerCmd) StartProtobufServer() error {
	address := n.Config.Server.InspectorListenAddress

	ln, err := net.Listen("tcp", address)
	if err != nil {
		n.Config.Log.Errorf("Failed to listen: %v", err)
		return err
	}
	defer ln.Close()

	go func() {
		n.Config.Log.Infof("Meshtastic protobuff inspector server listening on %s", address)
		for {
			conn, err := ln.Accept()
			if err == nil {
				go n.handleProtobuf(conn)
			}
		}
	}()

	n.Config.Log.Infof("Press CTRL+C to interrupt")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	<-stop
	n.Config.Log.Infof("Shutting down the inspector server gracefully...")
	return nil

}

func (n *ServerCmd) StartProxyServer() error {
	address := n.Config.Server.ProxyListenAddress
	backendAddress := n.Config.Server.ProxyForwardAddress

	poolSize := 200
	n.NewBackendConnectionPool(backendAddress, poolSize, 5*time.Second)

	listener, err := net.Listen("tcp", address)
	if err != nil {
		n.Config.Log.Fatal("listen error:", err)
	}
	proxyListener := &proxyproto.Listener{Listener: listener}
	defer func() {
		proxyListener.Close()
	}()

	n.Config.Log.Infof("🚀 Proxy server started and listening on %v with Proxy Protocol support", address)
	n.Config.Log.Infof("Forwarding connections to backend at %v", backendAddress)

	n.ConnMutex = sync.RWMutex{}
	n.ConnTrack = make(map[string]*ConnectionInfo)

	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil || conn.RemoteAddr() == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			go func(c net.Conn) {
				n.handleProxy(c)
			}(conn)
		}
	}()

	n.Config.Log.Infof("Proxy server is ready for connections")
	n.Config.Log.Infof("Press CTRL+C to interrupt")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	<-stop
	n.Config.Log.Infof("Shutting down the proxy server gracefully...")
	return nil
}
