package app

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/pkg/config"
)

var (
	ReleaseVersion = "v0.0.1-development"
	GitHash        = "0x0102c0de"
	ReleaseDate    = "2025-04-20"
)

type App struct {
	Config         *config.Config
	CommandBuilder *CmdBuilder
	RootCmd        *cobra.Command
	DefaultUsage   string
}

func NewApp(config *config.Config) (a *App) {
	a = new(App)
	a.RootCmd = new(cobra.Command)

	a.Config = config
	a.Config.Release.Date = ReleaseDate
	a.Config.Release.Version = ReleaseVersion
	a.Config.Release.Hash = GitHash

	a.RootCmd.Use = help.GlobalHelp(a.Config)

	a.RootCmd.SetHelpTemplate(a.RootCmd.Use)

	a.RegisterOsArgs()

	return a
}

func (a *App) Run() *App {

	a.ParseFlags()
	a.MapEnvVars()

	a.Config.SetupLogging()
	a.RootCmd.Execute()

	return a
}

func (a *App) MapEnvVars() {
	for _, v := range a.CommandBuilder.EnvMap {
		if os.Getenv(v.EnvVar) != "" {
			switch v.Type {
			case PString:
				*(v.Property.PString) = os.Getenv(v.EnvVar)
			case PBool:
				boolValue, err := strconv.ParseBool(os.Getenv(v.EnvVar))
				if err != nil {
					a.Config.Log.Fatalf("failed to parse %s boolean value %s: %s", v.EnvVar, os.Getenv(v.EnvVar), err)
				}
				*(v.Property.PBool) = boolValue
			case PInt:
				intValue, err := strconv.Atoi(os.Getenv(v.EnvVar))
				if err != nil {
					a.Config.Log.Fatalf("failed to parse %s integer value %s: %s", v.EnvVar, os.Getenv(v.EnvVar), err)
				}
				*(v.Property.PInt) = intValue
			}
		}
	}
}

func (a *App) ParseFlags() {
	a.RootCmd.ParseFlags(os.Args)
}

func (a *App) Destroy() {
	a.Config.Log.Info("exiting")
}
