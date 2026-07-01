package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newSwitchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "switch [name|host]",
		Short: "Point the stable front door at a siding, or `host` to run your local copy",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			var name string
			if len(args) == 1 {
				name = args[0]
			} else if name, err = pickSiding(app, true); err != nil {
				return err
			}
			return switchTo(ctx, &app, name)
		},
	}
}

// switchTo repoints the front door at a siding or the host, via siding.Switch
// (which stops the host app first, then either bridges a guest or steps the
// front door aside for the native app), and reports the result.
func switchTo(ctx context.Context, app *state.App, target string) error {
	if target != state.HostTarget {
		if _, ok := app.Sidings[target]; !ok {
			return fmt.Errorf("no siding %q (use `host` to run your local copy)", target)
		}
	}
	if err := siding.Switch(ctx, app, target); err != nil {
		return err
	}
	if target == state.HostTarget {
		fmt.Println("✓ switched to the host — started your local app; it now serves the front-door ports (Caddy stepped aside)")
		fmt.Printf("  switch back to a siding any time with `%s switch <name>`\n", bin())
		return nil
	}
	sd := app.Sidings[target]
	fmt.Printf("✓ switched to %q\n", target)
	for _, r := range app.FrontDoor {
		fmt.Printf("  localhost:%d  ->  %s:%d  (%s)\n", r.ListenPort, sd.LastIP, sd.Bridges[r.Key], r.Key)
	}
	return nil
}

// pickSiding lets the user choose a siding — arrow-key navigation on a TTY,
// numbered prompt otherwise.
func pickSiding(app state.App, includeHost bool) (string, error) {
	names := make([]string, 0, len(app.Sidings)+1)
	for n := range app.Sidings {
		names = append(names, n)
	}
	sort.Strings(names)
	if includeHost {
		// `host` (run the local copy) is a switch target too — list it first.
		names = append([]string{state.HostTarget}, names...)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no sidings yet — `%s new <name>`", bin())
	}
	if len(names) == 1 {
		return names[0], nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return pickSidingByNumber(app, names)
	}
	return pickSidingInteractive(app, names, fd)
}

// pickSidingInteractive is the arrow-key picker: ↑/↓ move, Enter selects, a digit
// jumps, q/Esc/Ctrl-C cancels. Starts on the live siding. Falls back to the
// number prompt if raw mode can't be set.
func pickSidingInteractive(app state.App, names []string, fd int) (string, error) {
	old, err := term.MakeRaw(fd)
	if err != nil {
		return pickSidingByNumber(app, names)
	}
	defer term.Restore(fd, old)

	sel := 0
	for i, n := range names {
		if n == app.LiveSiding {
			sel = i
		}
	}
	draw := func(first bool) {
		if !first {
			fmt.Fprintf(os.Stdout, "\x1b[%dA", len(names)+1) // back to the header line
		}
		fmt.Fprint(os.Stdout, "\rSelect a siding (↑/↓ move, Enter pick, number jumps, q quits):\x1b[K\r\n")
		for i, n := range names {
			marker := " "
			if n == app.LiveSiding {
				marker = "*"
			}
			if i == sel {
				fmt.Fprintf(os.Stdout, "\r\x1b[7m> %d) %s %s \x1b[0m\x1b[K\r\n", i+1, marker, n)
			} else {
				fmt.Fprintf(os.Stdout, "\r  %d) %s %s\x1b[K\r\n", i+1, marker, n)
			}
		}
	}
	draw(true)
	in := bufio.NewReader(os.Stdin)
	for {
		b, err := in.ReadByte()
		if err != nil {
			fmt.Fprint(os.Stdout, "\r\n")
			return "", err
		}
		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(os.Stdout, "\r\n")
			return names[sel], nil
		case b == 3 || b == 'q': // Ctrl-C / q
			fmt.Fprint(os.Stdout, "\r\n")
			return "", fmt.Errorf("cancelled")
		case b == 0x1b: // escape sequence — arrow keys: ESC [ A/B or ESC O A/B
			// (application-cursor mode). Read the next two bytes unconditionally;
			// a bare Esc is rare here (q / Ctrl-C cancel), and the old
			// Buffered()==0 guard mis-fired on arrows whose bytes weren't yet
			// buffered (e.g. under tmux/cmux), bailing instead of moving.
			b2, _ := in.ReadByte()
			b3, _ := in.ReadByte()
			if b2 == '[' || b2 == 'O' {
				if b3 == 'A' && sel > 0 {
					sel--
				} else if b3 == 'B' && sel < len(names)-1 {
					sel++
				}
			}
			draw(false)
		case b >= '1' && b <= '9':
			if i := int(b - '1'); i < len(names) {
				sel = i
				draw(false)
			}
		}
	}
}

// pickSidingByNumber is the non-TTY fallback: print the list, read a number.
func pickSidingByNumber(app state.App, names []string) (string, error) {
	fmt.Println("Select a siding:")
	for i, n := range names {
		marker := " "
		if app.LiveSiding == n {
			marker = "*"
		}
		fmt.Printf("  %d) %s %s\n", i+1, marker, n)
	}
	fmt.Print("> ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read selection: %w", err)
	}
	idx, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || idx < 1 || idx > len(names) {
		return "", fmt.Errorf("invalid selection %q", strings.TrimSpace(line))
	}
	return names[idx-1], nil
}
