package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show shunt's channel and resolved identity",
		RunE: func(cmd *cobra.Command, args []string) error {
			id := config.Current()
			fmt.Printf("shunt channel=%s binary=%s adminPort=%d portOffset=+%d containerPrefix=%s\n",
				id.Channel, id.BinaryName, id.AdminPort, id.PortOffset, id.ContainerPrefix)
			return nil
		},
	}
}
