//go:build darwin

package fsclone

import "golang.org/x/sys/unix"

func renamexSwap(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_SWAP)
}
