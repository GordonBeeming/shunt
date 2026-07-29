//go:build !darwin

package fsclone

import "fmt"

func renamexSwap(_, _ string) error {
	return fmt.Errorf("atomic directory swap requires Darwin")
}
