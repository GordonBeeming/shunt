package proc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type boundedCountingWriter struct {
	total    int64
	maxWrite int
}

func (w *boundedCountingWriter) Write(data []byte) (int, error) {
	w.total += int64(len(data))
	if len(data) > w.maxWrite {
		w.maxWrite = len(data)
	}
	return len(data), nil
}

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

func TestRunStreamingDoesNotRetainLargeOutput(t *testing.T) {
	if os.Getenv("SHUNT_PROC_STREAM_HELPER") == "1" {
		chunk := make([]byte, 32*1024)
		for written := 0; written < 16*1024*1024; written += len(chunk) {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(2)
			}
		}
		os.Exit(0)
	}

	w := &boundedCountingWriter{}
	cmd := os.Args[0]
	t.Setenv("SHUNT_PROC_STREAM_HELPER", "1")
	result, err := RunStreaming(context.Background(), w, io.Discard, cmd, "-test.run=TestRunStreamingDoesNotRetainLargeOutput")
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
	if w.total != 16*1024*1024 {
		t.Fatalf("streamed bytes = %d", w.total)
	}
	if w.maxWrite > 64*1024 {
		t.Fatalf("largest write = %d; output appears to be buffered", w.maxWrite)
	}
}

func TestRunStreamingHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RunStreaming(ctx, io.Discard, io.Discard, "sh", "-c", "printf unreachable")
	if err == nil {
		t.Fatal("RunStreaming() unexpectedly succeeded")
	}
}

func TestRunPipelineInDirStreamsProducerIntoConsumer(t *testing.T) {
	result, err := RunPipelineInDir(context.Background(), t.TempDir(), "sh", []string{"-c", "printf 'patch bytes'"}, "sh", []string{"-c", "tr '[:lower:]' '[:upper:]'"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "PATCH BYTES" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}

func TestRunPipelineInDirReportsProducerFailure(t *testing.T) {
	_, err := RunPipelineInDir(context.Background(), t.TempDir(), "sh", []string{"-c", "printf evidence >&2; exit 7"}, "sh", []string{"-c", "cat"})
	if err == nil || !strings.Contains(err.Error(), "exited 7: evidence") {
		t.Fatalf("error = %v", err)
	}
}
