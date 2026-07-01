package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill [name]",
		Short: "Stop a siding's guest (keeps its clone + data to restart later)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name, err := sidingArg(app, args)
			if err != nil {
				return err
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q", name)
			}
			if err := container.Stop(ctx, sd.Container); err != nil {
				return err
			}
			sd.Stopped = true
			app.Sidings[name] = sd
			if app.LiveSiding == name {
				fmt.Printf("⚠ %q was live — the front door now points at a stopped guest; switch to another siding\n", name)
			}
			if err := state.SaveApp(app); err != nil {
				return err
			}
			fmt.Printf("✓ stopped %q\n", name)
			return nil
		},
	}
}

func newRmCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "rm [name]",
		Short: "Tear down a siding: stop + remove the guest and delete its clone/data",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name, err := sidingArg(app, args)
			if err != nil {
				return err
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q", name)
			}
			if app.LiveSiding == name && !force {
				return fmt.Errorf("siding %q is live — switch away first, or pass --force", name)
			}
			fmt.Printf("• removing guest %q…\n", sd.Container)
			_ = container.Remove(ctx, sd.Container)

			src, _ := siding.Paths(app, name)
			// Tear down the git worktree + its branch from the main repo first,
			// so removing the dir doesn't leave a dangling worktree registration.
			fmt.Println("• removing the worktree…")
			_ = fsclone.RemoveWorktree(ctx, app.RepoPath, src, sd.Branch)
			base := filepath.Dir(src) // <configDir>/<name>
			// Guard against nuking an unintended path if ConfigDir/src was empty or
			// unresolved: only ever remove a deep, absolute, siding-shaped dir.
			if !filepath.IsAbs(base) || base == "/" || base == "." || filepath.Dir(base) == base {
				return fmt.Errorf("refusing to remove unsafe siding dir %q (resolved from %q)", base, src)
			}
			// Deleting the copy-on-write data clones is the slow part (a large SQL
			// volume can take a while), so say so instead of hanging silently.
			fmt.Println("• deleting siding data (a large data volume can take a while)…")
			if err := os.RemoveAll(base); err != nil {
				return fmt.Errorf("remove siding dir %s: %w", base, err)
			}
			delete(app.Sidings, name)
			if app.LiveSiding == name {
				app.LiveSiding = ""
			}
			if err := state.SaveApp(app); err != nil {
				return err
			}
			fmt.Printf("✓ removed %q\n", name)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "remove even if the siding is live")
	return c
}
