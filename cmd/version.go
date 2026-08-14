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
			fmt.Print(versionText())
			return nil
		},
	}
}

func versionText() string {
	id := config.Current()
	return fmt.Sprintf("shunt channel=%s binary=%s adminPort=%d portOffset=+%d containerPrefix=%s version=%s\n",
		id.Channel, id.BinaryName, id.AdminPort, id.PortOffset, id.ContainerPrefix, config.BuildVersion)
}
