package app

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/whereiskurt/meshtk/internal/app/nodeinfo"
	"github.com/whereiskurt/meshtk/pkg/config"
)

type CmdBuilder struct {
	RootCmd *cobra.Command
	Config  *config.Config
	EnvMap  []EnvPropertyMap
}

type EnvPropType string

const (
	PString EnvPropType = "string"
	PInt    EnvPropType = "integer"
	PBool   EnvPropType = "boolean"
)

type EnvPropPointer struct {
	PString *string
	PBool   *bool
	PInt    *int
}
type EnvPropertyMap struct {
	EnvVar   string
	Type     EnvPropType
	Property EnvPropPointer
}

func (a *App) RegisterOsArgs() {
	cmd := new(CmdBuilder)
	cmd.Config = a.Config
	cmd.RootCmd = a.RootCmd
	a.CommandBuilder = cmd

	cmd.GlobalS("LogFolder", &a.Config.LogFolder, []string{"l", "log"}, nil)
	cmd.GlobalS("HomeFolder", &a.Config.HomeFolder, []string{"d", "home"}, nil)
	cmd.GlobalS("VerboseLevel", &a.Config.VerboseLevel, []string{"v", "verbose"}, nil)
	cmd.GlobalS("ConfigFileName", &a.Config.ConfigFileName, []string{"c", "config"}, nil)

	ni := nodeinfo.NewNodeInfo(a.Config)
	nodeInfoCmd := cmd.NewCmd([]string{"nodeinfo", "n"}, ni.Help)
	cmd.NewSubCmd(nodeInfoCmd, "help", ni.Help)
	cmd.NewSubCmd(nodeInfoCmd, "announce", ni.Announce)

}

func (c *CmdBuilder) NewCmd(s []string, run func(*cobra.Command, []string)) *cobra.Command {
	return c.makeCmd(c.RootCmd, s, run)
}

func (c *CmdBuilder) NewSubCmd(parent *cobra.Command, s string, run func(*cobra.Command, []string)) *cobra.Command {
	return c.makeCmd(parent, []string{s}, run)
}

func (c *CmdBuilder) makeCmd(parent *cobra.Command, s []string, run func(*cobra.Command, []string)) *cobra.Command {
	if len(s) == 0 {
		panic("command must have a name")
	}

	aliases := []string{fmt.Sprintf("%ss", s[0])} // Add a pluralized alias
	if len(s) > 1 {
		aliases = append(aliases, s[1:]...)
	}

	child := &cobra.Command{
		Use:     s[0],
		Run:     run,
		PreRun:  parent.PreRun,
		Aliases: aliases,
	}

	parent.AddCommand(child)
	return child
}

func (c *CmdBuilder) GlobalB(name string, ref *bool, aliases []string, envalias []string) {
	c.FlagB(c.RootCmd, name, ref, aliases, envalias)
}

func (c *CmdBuilder) FlagB(cob *cobra.Command, name string, ref *bool, aliases []string, envalias []string) {
	cob.PersistentFlags().BoolVar(ref, name, *ref, "")
	_ = viper.BindPFlag(name, cob.PersistentFlags().Lookup(name))
	if len(aliases) > 0 && len(aliases[0]) == 1 {
		cob.PersistentFlags().Lookup(name).Shorthand = aliases[0]
	}
	for _, alias := range aliases {
		cob.PersistentFlags().BoolVar(ref, alias, *ref, "")
		cob.PersistentFlags().Lookup(alias).Hidden = true
		viper.RegisterAlias(alias, name)
	}
	for _, ev := range envalias {
		c.EnvMap = append(c.EnvMap,
			EnvPropertyMap{
				Type:     PString,
				Property: EnvPropPointer{PBool: ref},
				EnvVar:   ev})
	}
}

func (c *CmdBuilder) GlobalS(name string, ref *string, aliases []string, envalias []string) {
	c.FlagS(c.RootCmd, name, ref, aliases, envalias)
}

func (c *CmdBuilder) FlagS(cob *cobra.Command, name string, ref *string, aliases []string, envalias []string) {
	cob.PersistentFlags().StringVar(ref, name, *ref, "")
	_ = viper.BindPFlag(name, cob.PersistentFlags().Lookup(name))
	if len(aliases) > 0 && len(aliases[0]) == 1 {
		cob.PersistentFlags().Lookup(name).Shorthand = aliases[0]
	}
	for _, alias := range aliases {
		cob.PersistentFlags().StringVar(ref, alias, *ref, "")
		cob.PersistentFlags().Lookup(alias).Hidden = true
		viper.RegisterAlias(alias, name)
	}

	for _, ev := range envalias {
		c.EnvMap = append(c.EnvMap,
			EnvPropertyMap{
				Type:     PString,
				Property: EnvPropPointer{PString: ref},
				EnvVar:   ev})
	}
}

func (c *CmdBuilder) GlobalI(name string, ref *int, aliases []string, envalias []string) {
	c.FlagI(c.RootCmd, name, ref, aliases, envalias)
}

func (c *CmdBuilder) FlagI(cob *cobra.Command, name string, ref *int, aliases []string, envalias []string) {
	cob.PersistentFlags().IntVar(ref, name, *ref, "")
	_ = viper.BindPFlag(name, cob.PersistentFlags().Lookup(name))
	if len(aliases) > 0 && len(aliases[0]) == 1 {
		cob.PersistentFlags().Lookup(name).Shorthand = aliases[0]
	}
	for _, alias := range aliases {
		cob.PersistentFlags().IntVar(ref, alias, *ref, "")
		cob.PersistentFlags().Lookup(alias).Hidden = true
		viper.RegisterAlias(alias, name)
	}
	for _, ev := range envalias {
		c.EnvMap = append(c.EnvMap,
			EnvPropertyMap{
				Type:     PString,
				Property: EnvPropPointer{PInt: ref},
				EnvVar:   ev})
	}
}
