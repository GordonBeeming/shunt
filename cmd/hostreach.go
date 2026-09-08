package cmd

import (
	"log"
	"os"

	"github.com/gordonbeeming/shunt/internal/hostreach"
	"github.com/spf13/cobra"
)

// newHostReachCmd runs the host end of a siding's host-reach chain.
//
// Hidden because nothing types it: `up` starts it for a siding that declares
// hostReach, and `stop` ends it. It is a command rather than a separate binary
// so the host end ships with shunt and needs no build or install of its own.
func newHostReachCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "host-reach",
		Short:  "Run the host end of a siding's host-reach relay",
		Hidden: true,
	}
	c.AddCommand(newHostReachServeCmd())
	return c
}

func newHostReachServeCmd() *cobra.Command {
	var configPath string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Dial a siding's guest and carry its declared endpoints",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := hostreach.LoadHostConfig(configPath)
			if err != nil {
				return err
			}
			logger := log.New(os.Stderr, "", log.LstdFlags)
			return hostreach.Serve(cmd.Context(), cfg, logger)
		},
	}
	c.Flags().StringVar(&configPath, "config", "", "path to the host-reach config shunt wrote")
	_ = c.MarkFlagRequired("config")
	return c
}
