package protoserver

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/pkg/config"
)

type ProtoBufServerCmd struct {
	Config    *config.Config
	CmdOutput struct {
		WasSuccess bool
	}
}

func NewProtoBufServer(c *config.Config) (n *ProtoBufServerCmd) {
	n = new(ProtoBufServerCmd)
	n.Config = c

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

	n.StartInspectorServer()
}
