package siding

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gordonbeeming/shunt/internal/state"
)

var sidingNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidateName rejects values that cannot be used safely as one directory,
// branch suffix, and container-name component.
func ValidateName(name string) error {
	if name == state.HostTarget {
		return fmt.Errorf("%q is reserved for legacy state migration", name)
	}
	if !sidingNamePattern.MatchString(name) {
		return fmt.Errorf("invalid siding name %q: use letters, numbers, dots, underscores, or hyphens, starting with a letter or number", name)
	}
	return nil
}

// SidingBase returns the direct child directory for a siding. Every caller that
// derives a siding path goes through this check, so corrupt state cannot turn a
// cleanup or file operation into access outside the project config directory.
func SidingBase(app state.App, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	configDir := filepath.Clean(app.ConfigDir)
	if !filepath.IsAbs(configDir) {
		return "", fmt.Errorf("unsafe project config directory %q: expected an absolute path", app.ConfigDir)
	}
	base := filepath.Clean(filepath.Join(configDir, name))
	rel, err := filepath.Rel(configDir, base)
	if err != nil || rel != name || rel == "." || rel == ".." || filepath.IsAbs(rel) || filepath.Dir(rel) != "." {
		return "", fmt.Errorf("siding %q resolves outside project config directory %q", name, configDir)
	}
	return base, nil
}

// Paths returns the host worktree and data-root paths for one siding.
func Paths(app state.App, name string) (src, volRoot string, err error) {
	base, err := SidingBase(app, name)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(base, "src"), filepath.Join(base, "vol"), nil
}

// RemoveFiles deletes one validated siding directory without following a name
// supplied directly into os.RemoveAll.
func RemoveFiles(app state.App, name string) error {
	base, err := SidingBase(app, name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(base); err != nil {
		return fmt.Errorf("remove siding dir %s: %w", base, err)
	}
	return nil
}
