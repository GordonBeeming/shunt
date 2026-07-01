package ui

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// colorEnabled is resolved once at startup: colour only when stdout is a real
// terminal, NO_COLOR is unset (https://no-color.org), and TERM isn't "dumb". So
// piped/redirected output (incl. `--json`) and scripts stay plain automatically.
var colorEnabled = detectColor()

func detectColor() bool {
	// NO_COLOR disables colour whenever it's present, regardless of value (per
	// no-color.org) — so `NO_COLOR=` (empty) still counts.
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// paint wraps s in the SGR params when colour is enabled, else returns s as-is.
func paint(params, s string) string {
	if !colorEnabled || s == "" {
		return s
	}
	return "\x1b[" + params + "m" + s + "\x1b[0m"
}

// truecolor builds a 24-bit foreground SGR param string. Modern terminals on
// macOS support truecolor; the colorEnabled gate covers the rest.
func truecolor(r, g, b int) string { return fmt.Sprintf("38;2;%d;%d;%d", r, g, b) }

// Brand palette (dark variants — terminals are dark by default). See docs/brand.
// Green is the LIVE state ONLY and must never be used decoratively (brand rule).
func Cyan(s string) string { return paint(truecolor(0x46, 0xCB, 0xFF), s) } // #46CBFF — links, success, headings
func Live(s string) string { return paint(truecolor(0x6E, 0xE7, 0x9A), s) } // #6EE79A — the live marker/state, nothing else
func Bold(s string) string { return paint("1", s) }
func Dim(s string) string  { return paint("2", s) }
