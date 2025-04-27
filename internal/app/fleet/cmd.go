package fleet

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	internal "github.com/whereiskurt/meshtk/internal/mqtt"

	"github.com/spf13/cobra"

	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/pkg/config"
)

type FleetCmd struct {
	Config     *config.Config
	Nodes      []internal.NodeDB
	NodesMutex []sync.Mutex
	MqttClient []*internal.MqttClient
	CmdOutput  struct {
		WasSuccess bool
	}
}

const BACKSTOP_GRACE_SEC = 30

func NewFleet(c *config.Config) (f *FleetCmd) {
	f = new(FleetCmd)

	for i := 0; i < len(c.Fleet); i++ {
		f.Nodes = append(f.Nodes, make(internal.NodeDB))
		f.NodesMutex = append(f.NodesMutex, sync.Mutex{})
	}

	f.Config = c
	return f
}
func (f *FleetCmd) Help(cmd *cobra.Command, argz []string) {
	f.CmdOutput.WasSuccess = true
	fmt.Fprintln(f.Config.Stdout, help.FleetHelp(f.Config))
}

func (f *FleetCmd) Simulate(cmd *cobra.Command, argz []string) {
	f.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", f.Config)
	f.Config.Stdout.Write([]byte(s + "\n"))

	f.Config.Log.Trace("fleet.Simulate")
	f.Config.Log.Tracef("%+v", f.Config)

	for i := range f.Config.Fleet {
		f.initNodeDb(i)
		f.MqttClient = append(f.MqttClient, internal.NewMqttClient(f.Config, &f.Nodes[i]))

	}

	terminate := make(chan os.Signal, 1)

	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)

	alldone := make(chan int, len(f.Config.Fleet))

	// Start all of the fleet simulations
	for i := range f.Config.Fleet {
		go func(idx int) {
			fleetdone := make(chan bool)
			go func(idx int) {
				// Kick of the fleet simulation!
				f.MqttClient[idx].Connect()
				f.simulate(idx)
				f.MqttClient[idx].Disconnect()
				fleetdone <- true
				alldone <- idx
			}(idx)

			// Setup backstop timeouts to wait for the simulation to finish
			// This is to prevent the simulation from running indefinitely
			t := BACKSTOP_GRACE_SEC + f.Config.Fleet[idx].RampUpSecs + f.Config.Fleet[idx].RampSteadySecs + f.Config.Fleet[idx].RampDownSecs
			backstop := time.After(time.Duration(t) * time.Second)
			select {
			case <-fleetdone:
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Simulation completed successfully.\n", idx)))
			case <-backstop:
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Backstop timeout expired after %d seconds.\n", idx, t)))
				alldone <- idx
			}
		}(i)
	}

	// Hangout until all simulations are done or we get a termination signal
	select {
	case <-terminate:
		f.Config.Stdout.Write([]byte("\nReceived termination signal (CTRL+C)...\n"))
	case <-waitForAllCompletions(alldone, len(f.Config.Fleet)):
		f.Config.Stdout.Write([]byte("✅ All simulations completed.\n"))
	}

	f.Config.Stdout.Write([]byte("\n✅ Cleanly exiting ...\n"))

	for i := range f.Config.Fleet {
		f.flushNodeDb(i)
	}

	f.CmdOutput.WasSuccess = true
}

func waitForAllCompletions(completionChan chan int, count int) chan struct{} {
	done := make(chan struct{})
	go func() {
		for i := 0; i < count; i++ {
			<-completionChan
		}
		close(done)
	}()
	return done
}
