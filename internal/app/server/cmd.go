package server

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"

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

	ConnTrack map[string]*ConnectionInfo // maps connection ID to client ID
	ConnMutex sync.RWMutex

	Ciphers []cipher.Block
	Decider Decider // Interface for making packet routing decisions
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

	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal("Listen error:", err)
	}
	proxyListener := &proxyproto.Listener{Listener: listener}
	defer proxyListener.Close()

	n.Config.Log.Tracef("Listening on %v with Proxy Protocol support", address)

	n.ConnMutex = sync.RWMutex{}

	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil {
				log.Println("Accept error:", err)
				continue
			}
			go n.handleProxy(conn)
		}
	}()
	n.Config.Log.Infof("Press CTRL+C to interrupt")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	<-stop
	n.Config.Log.Infof("Shutting down the proxy server gracefully...")
	return nil
}
