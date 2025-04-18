package grpcserver

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/pkg/config"
)

type GRpcServerCmd struct {
	Config    *config.Config
	CmdOutput struct {
		WasSuccess bool
	}
}

func NewGRpcServer(c *config.Config) (n *GRpcServerCmd) {
	n = new(GRpcServerCmd)
	n.Config = c

	return n
}

func (n *GRpcServerCmd) Help(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	fmt.Fprintln(n.Config.Stdout, help.GRpcServerHelp(n.Config))
}
