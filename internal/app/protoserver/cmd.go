package protoserver

import (
	"fmt"
	"sync"

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
