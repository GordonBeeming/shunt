// Package resolve figures out which project (and siding) a shunt command refers
// to from the current working directory, git-style: run shunt from the repo or
// from inside a siding clone and it resolves the same project.
package resolve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gordonbeeming/shunt/internal/config"
)

// Location is the resolved project context.
type Location struct {
	Project   string // project (repo folder) name
	ConfigDir string // <repos>/.shunt[-channel]/<project>
	Siding    string // siding name if cwd is inside one, else ""
}

// FromCwd resolves the project from the current working directory.
func FromCwd() (Location, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Location{}, fmt.Errorf("get cwd: %w", err)
	}
	return From(cwd)
}

// From resolves the project from an arbitrary directory.
func From(dir string) (Location, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Location{}, fmt.Errorf("resolve %s: %w", dir, err)
	}
	dirName := config.Current().ProjectDirName

	// Case 1: cwd is inside …/<ProjectDirName>/<project>[/<siding>/…].
	parts := strings.Split(abs, string(os.PathSeparator))
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == dirName && i+1 < len(parts) {
			project := parts[i+1]
			configDir := string(os.PathSeparator) + filepath.Join(parts[1 : i+2]...)
			siding := ""
			if i+2 < len(parts) {
				siding = parts[i+2]
			}
			return Location{Project: project, ConfigDir: configDir, Siding: siding}, nil
		}
	}

	// Case 2: cwd is the repo itself — the config dir is the sibling.
	configDir, err := config.ProjectConfigDir(abs)
	if err != nil {
		return Location{}, err
	}
	return Location{Project: filepath.Base(abs), ConfigDir: configDir}, nil
}
