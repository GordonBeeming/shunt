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
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCloneVolumePreservesFilesystemIdentity(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	if err := os.Mkdir(source, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(source, "primary")
	if err := os.WriteFile(file, []byte("database-page"), 0o640); err != nil {
		t.Fatal(err)
	}
	hardLink := filepath.Join(source, "secondary")
	if err := os.Link(file, hardLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("primary", filepath.Join(source, "current")); err != nil {
		t.Fatal(err)
	}
	sparse, err := os.OpenFile(filepath.Join(source, "sparse"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sparse.Seek(1<<20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := sparse.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(file, "com.shunt.fidelity", []byte("preserve-me"), 0); err != nil {
		t.Fatal(err)
	}
	modified := time.Date(2025, 6, 7, 8, 9, 10, 123456000, time.Local)
	accessed := modified.Add(-time.Hour)
	if err := os.Chtimes(file, accessed, modified); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, accessed.Add(-time.Hour), modified.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	aclCommand := exec.Command("/bin/chmod", "+a", "everyone allow execute", file)
	if output, err := aclCommand.CombinedOutput(); err != nil {
		t.Fatalf("add ACL: %v: %s", err, output)
	}

	if err := CloneVolume(context.Background(), source, destination); err != nil {
		t.Fatal(err)
	}
	assertTreeFidelity(t, source, destination)

	rootInfo, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if got := rootInfo.Mode().Perm(); got != 0o750 {
		t.Fatalf("directory mode = %o, want 750", got)
	}
	primaryInfo, err := os.Lstat(filepath.Join(destination, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	if got := primaryInfo.Mode().Perm(); got != 0o640 {
		t.Fatalf("file mode = %o, want 640", got)
	}
	primaryStat := primaryInfo.Sys().(*syscall.Stat_t)
	secondaryInfo, err := os.Lstat(filepath.Join(destination, "secondary"))
	if err != nil {
		t.Fatal(err)
	}
	secondaryStat := secondaryInfo.Sys().(*syscall.Stat_t)
	if primaryStat.Ino != secondaryStat.Ino {
		t.Fatalf("hard links have inodes %d and %d", primaryStat.Ino, secondaryStat.Ino)
	}
	if got := primaryInfo.ModTime(); !got.Equal(modified) {
		t.Fatalf("modification time = %v, want %v", got, modified)
	}
	target, err := os.Readlink(filepath.Join(destination, "current"))
	if err != nil || target != "primary" {
		t.Fatalf("symlink target = %q, %v", target, err)
	}
	value := make([]byte, 32)
	size, err := unix.Getxattr(filepath.Join(destination, "primary"), "com.shunt.fidelity", value)
	if err != nil || string(value[:size]) != "preserve-me" {
		t.Fatalf("xattr = %q, %v", value[:max(size, 0)], err)
	}
	acl, err := readACL(context.Background(), filepath.Join(destination, "primary"))
	if err != nil || !strings.Contains(acl, "everyone allow execute") {
		t.Fatalf("ACL = %q, %v", acl, err)
	}
}

func TestCloneVolumeRejectsSetIDFileWithoutLeavingDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "set-id")
	if err := os.WriteFile(path, []byte("value"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	err := CloneVolume(context.Background(), source, destination)
	if err == nil || !strings.Contains(err.Error(), "set-id mode bits") {
		t.Fatalf("CloneVolume() error = %v", err)
	}
	if _, statErr := os.Lstat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("destination exists after rejected clone: %v", statErr)
	}
}

func TestCloneVolumeRejectsDestinationParentACL(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	parent := filepath.Join(root, "destination-parent")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/chmod", "+a", "everyone allow execute", parent)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("add parent ACL: %v: %s", err, output)
	}
	err := CloneVolume(context.Background(), source, filepath.Join(parent, "destination"))
	if err == nil || !strings.Contains(err.Error(), "could alter inherited permissions") {
		t.Fatalf("CloneVolume() error = %v", err)
	}
}

type testFileIdentity struct {
	mode       os.FileMode
	uid        uint32
	gid        uint32
	flags      uint32
	size       int64
	blocks     int64
	accessed   syscall.Timespec
	modified   syscall.Timespec
	born       syscall.Timespec
	linkTarget string
	xattrs     map[string][]byte
	acl        string
	hardLink   fileKey
}

func assertTreeFidelity(t *testing.T, sourceRoot, destinationRoot string) {
	t.Helper()
	source, err := captureTestTreeIdentity(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := captureTestTreeIdentity(destinationRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareTestTreeIdentity(source, destination); err != nil {
		t.Fatal(err)
	}
	if err := compareTestTreeContents(sourceRoot, destinationRoot); err != nil {
		t.Fatal(err)
	}
}

func captureTestTreeIdentity(root string) (map[string]testFileIdentity, error) {
	result := make(map[string]testFileIdentity)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		identity := testFileIdentity{}
		if info.Mode()&os.ModeSymlink != 0 {
			identity.linkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: unsupported stat result %T", path, info.Sys())
		}
		identity.mode = info.Mode()
		identity.uid = stat.Uid
		identity.gid = stat.Gid
		identity.flags = stat.Flags
		identity.size = info.Size()
		identity.blocks = stat.Blocks
		identity.accessed = stat.Atimespec
		identity.modified = stat.Mtimespec
		identity.born = stat.Birthtimespec
		identity.hardLink = fileKey{device: uint64(stat.Dev), inode: stat.Ino}
		identity.xattrs, err = readTestXattrs(path)
		if err != nil {
			return err
		}
		identity.acl, err = readACL(context.Background(), path)
		if err != nil {
			return err
		}
		result[relativePath] = identity
		return nil
	})
	return result, err
}

func readTestXattrs(path string) (map[string][]byte, error) {
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return map[string][]byte{}, nil
	}
	namesBuffer := make([]byte, size)
	size, err = unix.Llistxattr(path, namesBuffer)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]byte)
	for _, rawName := range bytes.Split(namesBuffer[:size], []byte{0}) {
		if len(rawName) == 0 {
			continue
		}
		name := string(rawName)
		valueSize, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			return nil, err
		}
		value := make([]byte, valueSize)
		if valueSize > 0 {
			read, err := unix.Lgetxattr(path, name, value)
			if err != nil {
				return nil, err
			}
			value = value[:read]
		}
		result[name] = value
	}
	return result, nil
}

func compareTestTreeIdentity(source, destination map[string]testFileIdentity) error {
	if len(source) != len(destination) {
		return fmt.Errorf("clone entry count = %d, want %d", len(destination), len(source))
	}
	sourceGroups := make(map[fileKey]string)
	destinationGroups := make(map[fileKey]string)
	paths := make([]string, 0, len(source))
	for path := range source {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		want := source[path]
		got, exists := destination[path]
		if !exists {
			return fmt.Errorf("clone is missing %q", path)
		}
		if got.mode != want.mode || got.uid != want.uid || got.gid != want.gid || got.flags != want.flags {
			return fmt.Errorf("%s mode, ownership, or flags differ", path)
		}
		if got.size != want.size || got.blocks != want.blocks {
			return fmt.Errorf("%s data shape differs", path)
		}
		if got.accessed != want.accessed || got.modified != want.modified || got.born != want.born {
			return fmt.Errorf("%s timestamps differ", path)
		}
		if got.linkTarget != want.linkTarget || got.acl != want.acl || !equalTestXattrs(got.xattrs, want.xattrs) {
			return fmt.Errorf("%s symlink, ACL, or xattrs differ", path)
		}
		if want.mode.IsRegular() {
			wantGroup, exists := sourceGroups[want.hardLink]
			if !exists {
				wantGroup = path
				sourceGroups[want.hardLink] = path
			}
			gotGroup, exists := destinationGroups[got.hardLink]
			if !exists {
				gotGroup = path
				destinationGroups[got.hardLink] = path
			}
			if gotGroup != wantGroup {
				return fmt.Errorf("%s hard-link relationship differs from %s", path, wantGroup)
			}
		}
	}
	return nil
}

func compareTestTreeContents(sourceRoot, destinationRoot string) error {
	return filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		relativePath, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		destination, err := os.ReadFile(filepath.Join(destinationRoot, relativePath))
		if err != nil {
			return err
		}
		if !bytes.Equal(source, destination) {
			return fmt.Errorf("%s contents differ", relativePath)
		}
		return nil
	})
}

func equalTestXattrs(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for name, value := range left {
		if !bytes.Equal(value, right[name]) {
			return false
		}
	}
	return true
}

func BenchmarkCloneVolumeNoCandidates(b *testing.B) {
	source := benchmarkVolume(b, 10_000)
	root := filepath.Dir(source)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		destination := filepath.Join(root, fmt.Sprintf("clone-%d", i))
		if err := CloneVolume(context.Background(), source, destination); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := os.RemoveAll(destination); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func BenchmarkRawClonefile(b *testing.B) {
	source := benchmarkVolume(b, 10_000)
	root := filepath.Dir(source)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		destination := filepath.Join(root, fmt.Sprintf("raw-%d", i))
		if err := unix.Clonefile(source, destination, cloneACL); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := os.RemoveAll(destination); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func benchmarkVolume(tb testing.TB, files int) string {
	tb.Helper()
	source := filepath.Join(tb.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < files; i++ {
		path := filepath.Join(source, fmt.Sprintf("file-%05d", i))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	return source
}
