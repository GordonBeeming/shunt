// Package ui renders terminal progress. LiveTail shows a status line plus the
// last few output lines in a fixed region that updates in place on a TTY, then
// collapses to a one-line summary when done — so long subprocess output (a
// dotnet build, image pulls) stays compact instead of scrolling off the screen.
package ui

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// LiveTail keeps a status line and a ring buffer of the last maxTail output
// lines. On a TTY it redraws the region in place and collapses it on Stop; on a
// non-TTY (piped/CI) it degrades to plain append-only output with no escapes.
type LiveTail struct {
	max        int
	tty        bool
	status     string
	tail       []string
	drawn      int    // rows drawn last render (TTY)
	lastStatus string // last status printed (non-TTY de-dup)
}

// NewLiveTail returns a tail that keeps the last maxTail lines visible.
func NewLiveTail(maxTail int) *LiveTail {
	return &LiveTail{max: maxTail, tty: isTTY()}
}

// Update sets the status line, appends any new non-empty output lines, and
// redraws the region once.
func (l *LiveTail) Update(status string, newLines []string) {
	l.status = status
	var added []string
	for _, ln := range newLines {
		if t := strings.TrimSpace(ln); t != "" {
			l.tail = append(l.tail, t)
			added = append(added, t)
		}
	}
	if len(l.tail) > l.max {
		l.tail = l.tail[len(l.tail)-l.max:]
	}

	if l.tty {
		l.draw()
		return
	}
	// Non-TTY: print new lines, and the status only when it changes.
	if status != "" && status != l.lastStatus {
		fmt.Println(status)
		l.lastStatus = status
	}
	for _, t := range added {
		fmt.Println("  │ " + t)
	}
}

// Stop collapses the live region to a single summary line.
func (l *LiveTail) Stop(summary string) {
	if l.tty && l.drawn > 0 {
		fmt.Printf("\033[%dA\r\033[2K", l.drawn) // jump to region top, clear line
		fmt.Println(summary)
		fmt.Print("\033[J") // wipe the rest of the old region
	} else {
		fmt.Println(summary)
	}
	l.drawn = 0
}

// Freeze leaves the current region on screen (no collapse) and stops tracking
// it, so subsequent output — e.g. an error message — prints below the last
// visible lines instead of overwriting them.
func (l *LiveTail) Freeze() {
	l.drawn = 0
}

func (l *LiveTail) rows() []string {
	out := make([]string, 0, l.max+1)
	if l.status != "" {
		out = append(out, l.status)
	}
	for _, t := range l.tail {
		out = append(out, "  │ "+t)
	}
	return out
}

func (l *LiveTail) draw() {
	rows := l.rows()
	if l.drawn > 0 {
		fmt.Printf("\033[%dA", l.drawn) // cursor up to region top
	}
	w := termWidth()
	for _, r := range rows {
		fmt.Printf("\033[2K%s\n", truncate(r, w-1)) // clear line, print (no wrap)
	}
	// The region only grows (status + capped ring buffer), so there are never
	// fewer rows than last time — no leftover lines to clear.
	l.drawn = len(rows)
}

func truncate(s string, max int) string {
	if max < 1 {
		max = 1
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func termWidth() int {
	type winsize struct{ row, col, x, y uint16 }
	ws := &winsize{}
	r, _, _ := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(ws)))
	if r == 0 && ws.col > 10 {
		return int(ws.col)
	}
	return 100
}
