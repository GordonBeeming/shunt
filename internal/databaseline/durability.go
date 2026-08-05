package databaseline

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func syncTreeDurable(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		err = fullSyncFile(file)
		closeErr := file.Close()
		if err != nil {
			return fmt.Errorf("sync baseline file %s: %w", path, err)
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := syncDirectoryDurable(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectoryDurable(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return fmt.Errorf("sync baseline directory %s: %w", path, syncErr)
	}
	return closeErr
}
