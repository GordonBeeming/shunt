//go:build !darwin

package databaseline

import "fmt"

func fscloneSwap(_, _ string) error {
	return fmt.Errorf("atomic directory swap requires Darwin")
}
