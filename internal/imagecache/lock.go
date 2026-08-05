package imagecache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const lockPollInterval = 15 * time.Millisecond

type storeLock struct {
	file *os.File
}

func acquireStoreLock(ctx context.Context, path string) (*storeLock, error) {
	return acquireCacheFileLock(ctx, path+".lock", "image cache", unix.LOCK_EX)
}

func acquireCacheFileLock(ctx context.Context, lockPath, description string, mode int) (*storeLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create image cache parent: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s lock: %w", description, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("chmod %s lock: %w", description, err)
	}

	ticker := time.NewTicker(lockPollInterval)
	defer ticker.Stop()
	for {
		err = unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return &storeLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock %s: %w", description, err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("wait for %s lock: %w", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

// withCacheUpdateLock serializes cache producers without blocking readers of
// the current immutable generation. A second cold-start waits here, then
// snapshots and reuses the generation published by the first instead of
// downloading/building the same sources and losing a publication race.
func withCacheUpdateLock(ctx context.Context, path string, fn func() error) (retErr error) {
	lock, err := acquireCacheFileLock(ctx, path+".update.lock", "image cache update", unix.LOCK_EX)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, lock.close()) }()
	return fn()
}

// withCacheSweepLock coordinates publishers with GC. Publishers hold it
// shared only around adoption/publication; collection holds it exclusively
// while scanning and deleting immutable objects, leaving ordinary readers free.
func withCacheSweepLock(ctx context.Context, path string, mode int, fn func() error) (retErr error) {
	lock, err := acquireCacheFileLock(ctx, path+".sweep.lock", "image cache sweep", mode)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, lock.close()) }()
	return fn()
}

func (lock *storeLock) close() error {
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}

func withStoreLock(ctx context.Context, path string, initialize bool, fn func() error) (retErr error) {
	lock, err := acquireStoreLock(ctx, path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, lock.close()) }()

	if err := ensureStoreRoot(path, initialize); err != nil {
		return err
	}
	return fn()
}

func ensureStoreRoot(path string, initialize bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat image cache: %w", err)
		}
		if !initialize {
			return os.ErrNotExist
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create image cache: %w", err)
		}
		return os.Chmod(path, 0o700)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("image cache path must not be a symlink")
	}
	if !info.IsDir() {
		if !initialize {
			return os.ErrNotExist
		}
		// Experimental archive layouts are deliberately not migrated. The
		// exact legacy cache file is disposable and can be fetched again.
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove legacy image cache: %w", err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("create image cache: %w", err)
		}
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("chmod image cache: %w", err)
	}
	return nil
}

// RepairPermissions is an explicit recovery operation for stores created by
// older builds or modified outside shunt. Ordinary cache operations set modes
// as they create or touch objects and must not walk historical generations.
func RepairPermissions(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("image cache contains symlink %s", path)
		}
		mode := os.FileMode(0o600)
		if entry.IsDir() {
			mode = 0o700
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm() == mode {
			return nil
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("repair image cache permissions for %s: %w", path, err)
		}
		return nil
	})
}
