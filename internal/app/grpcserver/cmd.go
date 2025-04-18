package grpcserver

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/pkg/config"
)

type GrpcServerCmd struct {
	Config    *config.Config
	CmdOutput struct {
		WasSuccess bool
	}
}

func NewGRpcServer(c *config.Config) (n *GrpcServerCmd) {
	n = new(GrpcServerCmd)
	n.Config = c

	return n
}

func (n *GrpcServerCmd) Help(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	fmt.Fprintln(n.Config.Stdout, help.GRpcServerHelp(n.Config))
}

func (n *GrpcServerCmd) Security(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("grpc.SecurityServer")
	n.Config.Log.Tracef("%+v", n.Config)

	n.StartServer()
}
