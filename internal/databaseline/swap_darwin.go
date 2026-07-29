//go:build darwin

package databaseline

import "golang.org/x/sys/unix"

func fscloneSwap(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_SWAP)
}
