//go:build !darwin

package databaseline

import "os"

func fullSyncFile(file *os.File) error { return file.Sync() }
