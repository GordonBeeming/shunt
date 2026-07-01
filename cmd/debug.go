package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/aspire"
	"github.com/spf13/cobra"
)

// newDebugDiscoverCmd is a hidden helper to exercise the Aspire discovery client
// against a (bridged) resource-service address during development.
func newDebugDiscoverCmd() *cobra.Command {
	var apiKey string
	c := &cobra.Command{
		Use:    "debug-discover <host:port>",
		Short:  "Probe a running Aspire app's resource service and print its endpoints",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eps, err := aspire.Discover(cmd.Context(), args[0], apiKey)
			if err != nil {
				return err
			}
			if len(eps) == 0 {
				fmt.Println("(no endpoints returned)")
				return nil
			}
			for _, e := range eps {
				fmt.Printf("%-18s %-8s %s://%s:%d  internal=%v\n",
					e.Resource, e.Name, e.Scheme, e.Host, e.Port, e.Internal)
			}
			return nil
		},
	}
	c.Flags().StringVar(&apiKey, "api-key", "", "resource service API key (if required)")
	return c
}
