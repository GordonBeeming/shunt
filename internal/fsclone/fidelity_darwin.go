//go:build darwin

package fsclone

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const cloneACL = 0x0004

type fileKey struct {
	device uint64
	inode  uint64
}

type cloneCandidate struct {
	relativePath string
	mode         os.FileMode
	hardLinked   bool
	hardLink     fileKey
	acl          string
}

type directoryTimes struct {
	accessed syscall.Timespec
	modified syscall.Timespec
}

type clonePlan struct {
	candidates     []cloneCandidate
	directoryTimes map[string]directoryTimes
}

func cloneVolumeTree(ctx context.Context, src, dest string) error {
	plan, err := buildClonePlan(ctx, src)
	if err != nil {
		return err
	}
	parentACL, err := readACL(ctx, filepath.Dir(dest))
	if err != nil {
		return fmt.Errorf("inspect clone destination parent ACL: %w", err)
	}
	if parentACL != "" {
		return fmt.Errorf("clone destination parent %q has an ACL that could alter inherited permissions", filepath.Dir(dest))
	}

	// clonefile performs the APFS copy-on-write operation in the kernel and
	// preserves modes, ownership (when permitted), timestamps, flags, xattrs,
	// resource forks, sparse allocation, and symlinks. The two documented gaps
	// relevant to Shunt are handled below: recursive cloning splits hard links,
	// and explicit ACLs are not reliably retained for descendants.
	if err := unix.Clonefile(src, dest, cloneACL); err != nil {
		return fmt.Errorf("clonefile: %w", err)
	}
	if err := repairHardLinks(ctx, dest, plan.candidates); err != nil {
		return err
	}
	if err := restoreCandidateACLs(ctx, dest, plan.candidates); err != nil {
		return err
	}
	if err := restoreDirectoryTimes(dest, plan.directoryTimes); err != nil {
		return err
	}
	return nil
}

func buildClonePlan(ctx context.Context, root string) (clonePlan, error) {
	// A native find traversal is the minimum production scan needed to preserve
	// hard-link identity: clonefile splits links and exposes no relationship
	// metadata afterward. The same pass selects the uncommon ACL and set-id
	// cases, avoiding Go walks and xattr reads for ordinary files.
	command := exec.CommandContext(ctx, "/usr/bin/find", root,
		"(", "-acl",
		"-o", "(", "-type", "f", "-links", "+1", ")",
		"-o", "(", "-type", "f", "(", "-perm", "-4000", "-o", "-perm", "-2000", ")", ")",
		")", "-print0")
	output, err := command.Output()
	if err != nil {
		return clonePlan{}, fmt.Errorf("scan clone fidelity candidates: %w", err)
	}

	var paths []string
	for _, rawPath := range bytes.Split(output, []byte{0}) {
		if len(rawPath) > 0 {
			paths = append(paths, string(rawPath))
		}
	}
	sort.Strings(paths)

	plan := clonePlan{directoryTimes: make(map[string]directoryTimes)}
	aclByHardLink := make(map[fileKey]string)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return clonePlan{}, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return clonePlan{}, fmt.Errorf("inspect clone candidate %s: %w", path, err)
		}
		if info.Mode().IsRegular() && info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return clonePlan{}, fmt.Errorf("clone candidate %q uses set-id mode bits that clonefile cannot preserve", path)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return clonePlan{}, fmt.Errorf("%s: unsupported stat result %T", path, info.Sys())
		}
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return clonePlan{}, err
		}
		candidate := cloneCandidate{relativePath: relativePath, mode: info.Mode()}
		if info.Mode().IsRegular() && stat.Nlink > 1 {
			candidate.hardLinked = true
			candidate.hardLink = fileKey{device: uint64(stat.Dev), inode: stat.Ino}
		}
		if candidate.hardLinked {
			acl, exists := aclByHardLink[candidate.hardLink]
			if !exists {
				acl, err = readACL(ctx, path)
				if err != nil {
					return clonePlan{}, fmt.Errorf("read ACL %s: %w", path, err)
				}
				aclByHardLink[candidate.hardLink] = acl
			}
			candidate.acl = acl
		} else {
			candidate.acl, err = readACL(ctx, path)
			if err != nil {
				return clonePlan{}, fmt.Errorf("read ACL %s: %w", path, err)
			}
		}
		plan.candidates = append(plan.candidates, candidate)
	}

	firstLink := make(map[fileKey]string)
	for _, candidate := range plan.candidates {
		if !candidate.hardLinked {
			continue
		}
		if _, exists := firstLink[candidate.hardLink]; !exists {
			firstLink[candidate.hardLink] = candidate.relativePath
			continue
		}
		parent := filepath.Dir(candidate.relativePath)
		if _, exists := plan.directoryTimes[parent]; exists {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, parent))
		if err != nil {
			return clonePlan{}, fmt.Errorf("inspect hard-link parent %s: %w", parent, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return clonePlan{}, fmt.Errorf("%s: unsupported stat result %T", parent, info.Sys())
		}
		plan.directoryTimes[parent] = directoryTimes{accessed: stat.Atimespec, modified: stat.Mtimespec}
	}
	return plan, nil
}

func readACL(ctx context.Context, path string) (string, error) {
	command := exec.CommandContext(ctx, "/bin/ls", "-lde", path)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ls -lde: %w: %s", err, strings.TrimSpace(string(output)))
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) <= 1 {
		return "", nil
	}
	return strings.Join(lines[1:], "\n"), nil
}

func repairHardLinks(ctx context.Context, destination string, candidates []cloneCandidate) error {
	first := make(map[fileKey]string)
	for _, candidate := range candidates {
		if !candidate.hardLinked {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		canonical, exists := first[candidate.hardLink]
		if !exists {
			first[candidate.hardLink] = candidate.relativePath
			continue
		}
		path := filepath.Join(destination, candidate.relativePath)
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("replace split hard link %s: %w", candidate.relativePath, err)
		}
		if err := os.Link(filepath.Join(destination, canonical), path); err != nil {
			return fmt.Errorf("restore hard link %s -> %s: %w", candidate.relativePath, canonical, err)
		}
	}
	return nil
}

func restoreCandidateACLs(ctx context.Context, destination string, candidates []cloneCandidate) error {
	restoredLinks := make(map[fileKey]struct{})
	for _, candidate := range candidates {
		if candidate.acl == "" {
			continue
		}
		if candidate.hardLinked {
			if _, restored := restoredLinks[candidate.hardLink]; restored {
				continue
			}
			restoredLinks[candidate.hardLink] = struct{}{}
		}
		if err := restoreACL(ctx, filepath.Join(destination, candidate.relativePath), candidate.mode, candidate.acl); err != nil {
			return fmt.Errorf("restore ACL for %s: %w", candidate.relativePath, err)
		}
	}
	return nil
}

func restoreACL(ctx context.Context, path string, mode os.FileMode, acl string) error {
	args := []string{"-E", path}
	if mode&os.ModeSymlink != 0 {
		args = []string{"-h", "-E", path}
	}
	var entries []string
	for _, line := range strings.Split(acl, "\n") {
		line = strings.TrimSpace(line)
		if separator := strings.Index(line, ": "); separator >= 0 {
			line = line[separator+2:]
		}
		entries = append(entries, line)
	}
	command := exec.CommandContext(ctx, "/bin/chmod", args...)
	command.Stdin = strings.NewReader(strings.Join(entries, "\n") + "\n")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("chmod -E: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func restoreDirectoryTimes(destination string, directories map[string]directoryTimes) error {
	for relativePath, times := range directories {
		path := filepath.Join(destination, relativePath)
		values := []unix.Timespec{
			{Sec: times.accessed.Sec, Nsec: times.accessed.Nsec},
			{Sec: times.modified.Sec, Nsec: times.modified.Nsec},
		}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, values, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("restore hard-link parent timestamps for %s: %w", relativePath, err)
		}
	}
	return nil
}
