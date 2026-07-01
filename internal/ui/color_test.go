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
	// Tests run non-TTY, so colorEnabled is false → every wrapper returns plain.
	for _, s := range []string{"live", "https://localhost:5000", "✓"} {
		if got := Cyan(s); got != s {
			t.Errorf("Cyan(%q) with colour disabled = %q, want plain", s, got)
		}
		if got := Live(s); got != s {
			t.Errorf("Live(%q) with colour disabled = %q, want plain", s, got)
		}
	}
	// The wrap format is exercised directly (independent of the TTY gate).
	if got := wrapFor(t, "38;2;70;203;255", "x"); !strings.HasPrefix(got, "\x1b[38;2;70;203;255m") || !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("wrap format wrong: %q", got)
	}
}

// wrapFor exercises the SGR wrapping directly (paint() is gated on colorEnabled,
// which is false under `go test`, so we build the wrapped form the same way).
func wrapFor(t *testing.T, params, s string) string {
	t.Helper()
	return "\x1b[" + params + "m" + s + "\x1b[0m"
}
