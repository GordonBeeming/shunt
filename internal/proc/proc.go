// Package proc is the single place shunt shells out to external CLIs
// (container, caddy, xcaddy, git, cp, launchctl). Keeping it here makes the
// process boundary consistent and easy to reason about.
package proc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// When shunt runs under a launchd agent (the dashboard LaunchAgent), the process
// inherits a minimal PATH (roughly /usr/bin:/bin:/usr/sbin:/sbin) that excludes
// the usual install dirs — so a bare `container`/`docker` (system installs) or
// `aspire`/dotnet global tools (under $HOME) fail to exec even though they're
// installed. A host-side `sh -lc` inherits this PATH too, and macOS path_helper
// preserves appended entries, so fixing PATH once at startup covers every
// subprocess here, regardless of who launched shunt.
func init() {
	dirs := []string{"/usr/local/bin", "/opt/homebrew/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		// The Aspire CLI (~/.aspire/bin) and dotnet global tools (~/.dotnet/tools)
		// live under $HOME and aren't on launchd's PATH or in /etc/paths.d, so the
		// host runner (`aspire start`) fails from the dashboard agent without these.
		dirs = append(dirs, filepath.Join(home, ".dotnet", "tools"), filepath.Join(home, ".aspire", "bin"))
	}
	_ = os.Setenv("PATH", augmentPath(os.Getenv("PATH"), dirs, dirExists))
}

func dirExists(d string) bool {
	fi, err := os.Stat(d)
	return err == nil && fi.IsDir()
}

// augmentPath appends each of dirs that exists and isn't already on path. Order
// is preserved and existing entries win, so this only adds fallbacks — it never
// shadows a binary the caller could already resolve.
func augmentPath(path string, dirs []string, exists func(string) bool) string {
	present := make(map[string]bool)
	for _, d := range strings.Split(path, string(os.PathListSeparator)) {
		present[d] = true
	}
	for _, d := range dirs {
		if present[d] || !exists(d) {
			continue
		}
		// Don't prepend a separator to an empty PATH — a leading separator makes an
		// empty element, which Unix treats as the current directory (a security risk).
		if path == "" {
			path = d
		} else {
			path += string(os.PathListSeparator) + d
		}
		present[d] = true
	}
	return path
}

// Result captures a finished command's output and exit status.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes name with args and captures stdout/stderr. A non-zero exit is
// returned as an error that includes stderr, so callers get a useful message
// without having to re-assemble it.
func Run(ctx context.Context, name string, args ...string) (Result, error) {
	return RunInDir(ctx, "", name, args...)
}

// RunInDir is Run with the process's working directory set to dir (empty = the
// current one). Use it instead of injecting a `cd <dir> && …` into a shell
// string — a shell would subject dir to expansion (quotes don't stop `$`/backtick
// evaluation in sh), whereas cmd.Dir is passed verbatim to the OS.
func RunInDir(ctx context.Context, dir, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			return res, fmt.Errorf("%s exited %d: %s", name, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		return res, fmt.Errorf("run %s: %w", name, err)
	}
	return res, nil
}

// RunPassthrough runs a command with stdio wired straight to the user's
// terminal — for long/interactive operations (image builds, dotnet run) where
// streaming output matters more than capturing it.
func RunPassthrough(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}

// RunToFile runs name with args, streaming stdout straight to the file at
// outPath (created/truncated). For large binary output — e.g. `docker save` of
// multi-GB images — where buffering into memory would be wrong. Stderr is
// captured so a failure still produces a useful message.
func RunToFile(ctx context.Context, outPath, name string, args ...string) error {
	return RunToFileLimited(ctx, outPath, 0, name, args...)
}

// RunToFileLimited is RunToFile with a hard byte ceiling. A positive limit
// stops a producer before untrusted output can grow the destination without
// bound; zero retains RunToFile's unlimited behavior.
func RunToFileLimited(ctx context.Context, outPath string, maxBytes int64, name string, args ...string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	if maxBytes > 0 {
		cmd.Stdout = &limitedWriter{writer: f, remaining: maxBytes}
	} else {
		cmd.Stdout = f
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, fmt.Errorf("output exceeds configured byte limit")
	}
	if int64(len(data)) > w.remaining {
		allowed := int(w.remaining)
		written, err := w.writer.Write(data[:allowed])
		w.remaining -= int64(written)
		if err != nil {
			return written, err
		}
		return written, fmt.Errorf("output exceeds configured byte limit")
	}
	written, err := w.writer.Write(data)
	w.remaining -= int64(written)
	return written, err
}

// RunStdin runs name with args, feeding the file at stdinPath as stdin — for
// streaming a large file into a command (e.g. `docker load` of a multi-GB tar)
// without buffering it. Stderr is captured for a useful error.
func RunStdin(ctx context.Context, stdinPath, name string, args ...string) error {
	_, err := RunStdinDigest(ctx, stdinPath, name, args...)
	return err
}

// RunStdinDigest streams a file into a command while computing its SHA-256.
// It avoids a separate full-file verification pass before the command consumes
// the bytes; callers compare the returned digest with their trusted metadata.
func RunStdinDigest(ctx context.Context, stdinPath, name string, args ...string) (string, error) {
	f, err := os.Open(stdinPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", stdinPath, err)
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	hash := sha256.New()
	cmd.Stdin = io.TeeReader(f, hash)
	cmd.Stdout = os.Stdout // surface progress (e.g. `docker load` "Loaded image:" lines)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("run %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// Look reports whether an executable is on PATH.
func Look(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
