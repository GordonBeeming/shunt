package cmd

import (
	"fmt"
	"strings"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/spf13/cobra"
)

// newConfigCmd groups shunt user-config commands. Config is per channel, stored
// at <global-dir>/config.json; a project's .shunt.app.json can override per app.
func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Get/set shunt user config (defaults the contract can override per app)",
	}
	c.AddCommand(
		configField("branchPrefix", "siding worktree branch prefix, e.g. gb/shunt/",
			func(u *config.UserConfig) *string { return &u.BranchPrefix }, config.BranchPrefix, validateBranchPrefix),
		configField("memory", "default per-guest RAM cap, e.g. 4g (bump heavy apps in their contract)",
			func(u *config.UserConfig) *string { return &u.Memory }, config.GuestMemory, nil),
		configField("cpus", "default per-guest CPU cap, e.g. 4",
			func(u *config.UserConfig) *string { return &u.CPUs }, config.GuestCPUs, nil),
	)
	return c
}

// validateBranchPrefix rejects a prefix that would mash straight into the siding
// name. The branch is built as `prefix + name`, so the prefix must end in a
// separator (`/` or `-`) or `gb/shunt` + `foo` silently becomes `gb/shuntfoo`.
// Empty is allowed — it clears the override back to the default.
func validateBranchPrefix(v string) error {
	if v == "" || strings.HasSuffix(v, "/") || strings.HasSuffix(v, "-") {
		return nil
	}
	return fmt.Errorf("branch prefix %q must end in %q or %q — it's joined straight onto the siding name, so %q would become e.g. %q, not %q",
		v, "/", "-", v, v+"my-siding", v+"/my-siding")
}

// configField builds a `shunt config <name> [value]` get/set command for one
// string field; `effective` prints the value-in-effect (with default applied).
// validate (optional) rejects a bad value before it's saved.
func configField(name, desc string, field func(*config.UserConfig) *string, effective func() string, validate func(string) error) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [value]",
		Short: "Get or set the " + desc,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Println(effective())
				return nil
			}
			if validate != nil {
				if err := validate(args[0]); err != nil {
					return err
				}
			}
			uc := config.LoadUserConfig()
			*field(&uc) = args[0]
			if err := config.SaveUserConfig(uc); err != nil {
				return err
			}
			fmt.Printf("%s %s = %q\n", tick(), name, args[0])
			fmt.Println("  applies to sidings created from now on")
			return nil
		},
	}
}
