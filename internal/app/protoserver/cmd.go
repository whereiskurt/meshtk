package protoserver

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
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
)

type ProtoBufServerCmd struct {
	Config    *config.Config
	CmdOutput struct {
		WasSuccess bool
	}

	ConnTrack map[string]*ConnectionInfo // maps connection ID to client ID
	ConnMutex sync.RWMutex

	Ciphers []cipher.Block
}

func NewAESCipher(key []byte) cipher.Block {
	c, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	return c
}

func NewServer(c *config.Config) (n *ProtoBufServerCmd) {
	n = new(ProtoBufServerCmd)
	n.Config = c
	n.ConnTrack = make(map[string]*ConnectionInfo)
	n.freeConnTrack()

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

	return n
}

func (n *ProtoBufServerCmd) Help(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	fmt.Fprintln(n.Config.Stdout, help.ProtoBufServerHelp(n.Config))
}

func (n *ProtoBufServerCmd) ProtobufServer(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("protobuf.InspectorServer")
	n.Config.Log.Tracef("%+v", n.Config)

	n.StartProtobufServer()
}

func (n *ProtoBufServerCmd) ProxyServer(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("protobuf.ProxyServer")
	n.Config.Log.Tracef("%+v", n.Config)

	n.StartProxyServer()
}

func (n *ProtoBufServerCmd) StartProtobufServer() error {
	address := n.Config.ProtoBufServer.InspectorListenAddress

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

func (n *ProtoBufServerCmd) StartProxyServer() error {
	address := n.Config.ProtoBufServer.ProxyListenAddress

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
