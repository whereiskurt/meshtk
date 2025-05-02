package protoserver

import (
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

// ConnectionInfo stores information about a client connection
type ConnectionInfo struct {
	ClientID    string
	Username    string
	Password    string
	IPAddress   string
	ConnectTime int64
}

type ProtoBufServerCmd struct {
	Config    *config.Config
	CmdOutput struct {
		WasSuccess bool
	}

	// Connection tracking
	connectionsMutex sync.RWMutex
	clientIDByConnID map[string]*ConnectionInfo // maps connection ID to client ID
}

func NewProtoBufServer(c *config.Config) (n *ProtoBufServerCmd) {
	n = new(ProtoBufServerCmd)
	n.Config = c
	n.clientIDByConnID = make(map[string]*ConnectionInfo)

	return n
}

func (n *ProtoBufServerCmd) Help(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	fmt.Fprintln(n.Config.Stdout, help.ProtoBufServerHelp(n.Config))
}

func (n *ProtoBufServerCmd) Inspector(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("protobuf.InspectorServer")
	n.Config.Log.Tracef("%+v", n.Config)

	n.StartProtobufServer()
}

func (n *ProtoBufServerCmd) Proxy(cmd *cobra.Command, argz []string) {
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

	n.Config.Log.Tracef("Listening on %v with Proxy Protocol", address)

	n.connectionsMutex = sync.RWMutex{}

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
