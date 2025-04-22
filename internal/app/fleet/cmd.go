package fleet

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/pkg/config"
)

type FleetCmd struct {
	Config    *config.Config
	CmdOutput struct {
		WasSuccess bool
	}
}

func NewFleet(c *config.Config) (f *FleetCmd) {
	f = new(FleetCmd)
	f.Config = c
	return f
}
func (n *FleetCmd) Help(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	fmt.Fprintln(n.Config.Stdout, help.FleetHelp(n.Config))
}

func (n *FleetCmd) Simulate(cmd *cobra.Command, argz []string) {
	n.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", n.Config)
	n.Config.Stdout.Write([]byte(s + "\n"))

	n.Config.Log.Trace("fleet.Simulate")
	n.Config.Log.Tracef("%+v", n.Config)

}
