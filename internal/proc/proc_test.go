package proc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAugmentPath(t *testing.T) {
	exists := func(d string) bool { return d == "/usr/local/bin" || d == "/opt/homebrew/bin" }

	cases := []struct {
		desc string
		path string
		dirs []string
		want string
	}{
		{
			desc: "appends missing install dirs that exist",
			path: "/usr/bin:/bin",
			dirs: []string{"/usr/local/bin", "/opt/homebrew/bin"},
			want: "/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
		},
		{
			desc: "skips dirs already present",
			path: "/usr/local/bin:/usr/bin",
			dirs: []string{"/usr/local/bin", "/opt/homebrew/bin"},
			want: "/usr/local/bin:/usr/bin:/opt/homebrew/bin",
		},
		{
			desc: "skips dirs that don't exist",
			path: "/usr/bin",
			dirs: []string{"/nope/bin"},
			want: "/usr/bin",
		},
		{
			desc: "empty PATH gets no leading separator (no empty cwd element)",
			path: "",
			dirs: []string{"/usr/local/bin", "/opt/homebrew/bin"},
			want: "/usr/local/bin:/opt/homebrew/bin",
		},
		{
			desc: "no change when all present",
			path: "/usr/local/bin:/opt/homebrew/bin",
			dirs: []string{"/usr/local/bin", "/opt/homebrew/bin"},
			want: "/usr/local/bin:/opt/homebrew/bin",
		},
	}
	for _, c := range cases {
		if got := augmentPath(c.path, c.dirs, exists); got != c.want {
			t.Errorf("%s: augmentPath = %q, want %q", c.desc, got, c.want)
		}
	}
}

func TestRunStdinDigestHashesTheBytesConsumedByCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	payload := []byte("streamed once into the command")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := RunStdinDigest(context.Background(), path, "sh", "-c", "cat >/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("sha256:%x", sha256.Sum256(payload))
	if digest != want {
		t.Fatalf("digest = %q, want %q", digest, want)
	}
}

func TestRunToFileLimitedStopsOversizedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	err := RunToFileLimited(context.Background(), path, 4, "sh", "-c", "printf 12345678")
	if err == nil || !strings.Contains(err.Error(), "configured byte limit") {
		t.Fatalf("RunToFileLimited() error = %v", err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Size() > 4 {
		t.Fatalf("limited output size = %d", info.Size())
	}
}
