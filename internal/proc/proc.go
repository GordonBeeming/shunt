// Package proc is the single place shunt shells out to external CLIs
// (container, caddy, xcaddy, git, cp, launchctl). Keeping it here makes the
// process boundary consistent and easy to reason about.
package proc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrPipelineInputLimit reports that a producer emitted more bytes than a
// pipeline caller allowed. Callers can use errors.Is to distinguish it from a
// process failure.
var ErrPipelineInputLimit = errors.New("pipeline input exceeds configured byte limit")

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

// RunStreaming executes name with stdout and stderr sent directly to the
// supplied writers. It is intended for output that may be too large to retain
// in memory, such as a binary git diff.
func RunStreaming(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	res := Result{}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			return res, fmt.Errorf("%s exited %d", name, res.ExitCode)
		}
		return res, fmt.Errorf("run %s: %w", name, err)
	}
	return res, nil
}

// RunPipelineInDir connects the stdout of producer directly to the stdin of
// consumer and captures only the consumer's stdout. It avoids retaining large
// intermediate values such as binary Git patches in memory.
func RunPipelineInDir(ctx context.Context, dir string, producerName string, producerArgs []string, consumerName string, consumerArgs []string) (Result, error) {
	return RunPipelineInDirLimited(ctx, dir, 0, producerName, producerArgs, consumerName, consumerArgs)
}

// RunPipelineInDirLimited is RunPipelineInDir with a hard ceiling on bytes
// copied from producer to consumer. Zero keeps the unlimited behavior.
func RunPipelineInDirLimited(ctx context.Context, dir string, maxInputBytes int64, producerName string, producerArgs []string, consumerName string, consumerArgs []string) (Result, error) {
	pipeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	producer := exec.CommandContext(pipeCtx, producerName, producerArgs...)
	producer.Dir = dir
	consumer := exec.CommandContext(pipeCtx, consumerName, consumerArgs...)
	consumer.Dir = dir

	producerOut, err := producer.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("pipe %s stdout: %w", producerName, err)
	}
	consumerIn, err := consumer.StdinPipe()
	if err != nil {
		return Result{}, joinPipelineErrors(fmt.Errorf("pipe %s stdin: %w", consumerName, err), labelledClose("producer stdout", producerOut.Close()))
	}
	var producerStderr, consumerStdout, consumerStderr bytes.Buffer
	producer.Stderr = &producerStderr
	consumer.Stdout = &consumerStdout
	consumer.Stderr = &consumerStderr

	if err := consumer.Start(); err != nil {
		return Result{}, errors.Join(
			fmt.Errorf("consumer start %s: %w", consumerName, err),
			labelledClose("producer stdout", producerOut.Close()),
			labelledClose("consumer stdin", consumerIn.Close()),
		)
	}
	if err := producer.Start(); err != nil {
		cancel()
		closeErr := consumerIn.Close()
		producerCloseErr := producerOut.Close()
		waitErr := consumer.Wait()
		return Result{}, errors.Join(
			fmt.Errorf("producer start %s: %w", producerName, err),
			labelledClose("producer stdout", producerCloseErr),
			labelledCleanup("consumer", closeErr, waitErr),
		)
	}

	type pipelineDone struct{ copyErr, waitErr error }
	producerDone := make(chan pipelineDone, 1)
	consumerDone := make(chan error, 1)
	go func() {
		copyErr := copyPipeline(consumerIn, producerOut, maxInputBytes)
		if copyErr != nil {
			cancel()
		}
		producerDone <- pipelineDone{copyErr: copyErr, waitErr: producer.Wait()}
	}()
	go func() { consumerDone <- consumer.Wait() }()

	var producerResult pipelineDone
	var producerReceived bool
	var consumerErr error
	var consumerReceived bool
	select {
	case producerResult = <-producerDone:
		producerReceived = true
		if producerResult.copyErr != nil || producerResult.waitErr != nil {
			cancel()
		}
	case consumerErr = <-consumerDone:
		consumerReceived = true
		// A successful consumer may be observed before the producer waiter runs
		// even after the producer has closed stdout. Cancelling here races that
		// normal completion and turns a valid pipeline into context cancellation.
		// Failed consumers still need to stop a producer that could be blocked on
		// a full pipe.
		if consumerErr != nil {
			cancel()
		}
	}
	if !producerReceived {
		producerResult = <-producerDone
	}
	if !consumerReceived {
		consumerErr = <-consumerDone
	}
	result := Result{Stdout: consumerStdout.String(), Stderr: consumerStderr.String(), ExitCode: exitCode(consumer)}
	var errs []error
	if producerResult.copyErr != nil {
		errs = append(errs, fmt.Errorf("pipeline copy: %w", producerResult.copyErr))
	}
	if producerResult.waitErr != nil {
		errs = append(errs, commandFailure("producer", producerName, producer, producerStderr.String(), producerResult.waitErr))
	}
	if consumerErr != nil {
		errs = append(errs, commandFailure("consumer", consumerName, consumer, consumerStderr.String(), consumerErr))
	}
	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

func copyPipeline(dst io.WriteCloser, src io.ReadCloser, maxBytes int64) (result error) {
	defer func() {
		result = errors.Join(result, labelledClose("consumer stdin", dst.Close()), labelledClose("producer stdout", src.Close()))
	}()
	buf := make([]byte, 32*1024)
	var copied int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if maxBytes > 0 && copied+int64(n) > maxBytes {
				return ErrPipelineInputLimit
			}
			written, writeErr := dst.Write(buf[:n])
			copied += int64(written)
			if writeErr != nil {
				return fmt.Errorf("write consumer stdin: %w", writeErr)
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("read producer stdout: %w", readErr)
		}
	}
}

func labelledClose(name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close %s: %w", name, err)
}

func commandFailure(role, name string, cmd *exec.Cmd, stderr string, err error) error {
	return fmt.Errorf("%s %s exited %d: %s: %w", role, name, exitCode(cmd), strings.TrimSpace(stderr), err)
}

func labelledCleanup(role string, closeErr, waitErr error) error {
	var errs []error
	if closeErr != nil {
		errs = append(errs, fmt.Errorf("%s cleanup close: %w", role, closeErr))
	}
	if waitErr != nil {
		errs = append(errs, fmt.Errorf("%s cleanup wait: %w", role, waitErr))
	}
	return errors.Join(errs...)
}

func joinPipelineErrors(primary error, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	return errors.Join(primary, cleanup)
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
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
