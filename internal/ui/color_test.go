package ui

import (
	"strings"
	"testing"
)

func TestTruecolor(t *testing.T) {
	if got := truecolor(0x46, 0xCB, 0xFF); got != "38;2;70;203;255" {
		t.Errorf("truecolor(#46CBFF) = %q, want 38;2;70;203;255", got)
	}
}

func TestPaint(t *testing.T) {
	// Drive colorEnabled explicitly so the test is deterministic regardless of
	// whether `go test` runs attached to a terminal.
	saved := colorEnabled
	defer func() { colorEnabled = saved }()

	colorEnabled = false
	for _, s := range []string{"live", "https://localhost:5000", "✓"} {
		if got := Cyan(s); got != s {
			t.Errorf("Cyan(%q) with colour disabled = %q, want plain", s, got)
		}
		if got := Live(s); got != s {
			t.Errorf("Live(%q) with colour disabled = %q, want plain", s, got)
		}
	}

	colorEnabled = true
	if got := Cyan("x"); !strings.HasPrefix(got, "\x1b[38;2;70;203;255m") || !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("Cyan enabled = %q, want cyan-wrapped", got)
	}
	if got := Live("x"); !strings.HasPrefix(got, "\x1b[38;2;110;231;154m") {
		t.Errorf("Live enabled = %q, want green-wrapped", got)
	}
	if got := paint("38;2;70;203;255", ""); got != "" {
		t.Errorf("paint of empty string = %q, want empty (no wrapping)", got)
	}
}
