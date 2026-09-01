// Package runner classifies how an app starts — .NET Aspire, plain .NET, Node,
// or a custom command — so shunt can run any repo, not just Aspire. The runner
// decides only how the app is launched: every runner, Aspire included, declares
// a guest port per front-door route rather than having one discovered for it.
package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Kinds.
const (
	Aspire = "aspire"
	Dotnet = "dotnet"
	Node   = "node"
	Custom = "custom"
)

// Detected describes how to start an app. Start/Workdir are empty for aspire
// (the start command is derived from the AppHost path).
type Detected struct {
	Kind    string
	Start   string // command to run in the guest (non-aspire)
	Workdir string // dir to run Start in, relative to repo root (non-aspire)
}

// Detect classifies a repo by what's on disk. Order matters: an Aspire repo also
// has .csproj/package.json, so aspire wins, then node (package.json), then dotnet
// (first .csproj). Returns Kind=Custom (with empty Start) when it can't classify —
// the caller asks.
func Detect(repoPath string) Detected {
	if findAppHost(repoPath) != "" {
		return Detected{Kind: Aspire}
	}
	if pkg := filepath.Join(repoPath, "package.json"); exists(pkg) {
		return Detected{Kind: Node, Start: nodeStart(pkg)}
	}
	if csproj := findFirstCsproj(repoPath); csproj != "" {
		return Detected{Kind: Dotnet, Start: "dotnet run", Workdir: filepath.Dir(rel(repoPath, csproj))}
	}
	return Detected{Kind: Custom}
}

// findAppHost returns the first Aspire AppHost csproj (by name or SDK ref), or "".
func findAppHost(root string) string {
	var found string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".csproj") {
			return nil
		}
		if strings.Contains(strings.ToLower(filepath.Base(p)), "apphost") {
			found = p
			return nil
		}
		if b, e := os.ReadFile(p); e == nil && strings.Contains(string(b), "Aspire.AppHost.Sdk") {
			found = p
		}
		return nil
	})
	return found
}

func findFirstCsproj(root string) string {
	var found string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".csproj") {
			found = p
		}
		return nil
	})
	return found
}

// nodeStart picks the package manager (pnpm/yarn/npm) and the dev/start script.
func nodeStart(pkgPath string) string {
	dir := filepath.Dir(pkgPath)
	script := "dev"
	if b, err := os.ReadFile(pkgPath); err == nil {
		var p struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &p) == nil {
			if _, ok := p.Scripts["dev"]; !ok {
				if _, ok := p.Scripts["start"]; ok {
					script = "start"
				}
			}
		}
	}
	switch {
	case exists(filepath.Join(dir, "pnpm-lock.yaml")):
		return "pnpm " + script
	case exists(filepath.Join(dir, "yarn.lock")):
		return "yarn " + script
	default:
		return "npm run " + script
	}
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "bin", "obj", ".shunt", ".shunt-dev", ".shunt-beta", ".shunt-nightly":
		return true
	}
	return false
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func rel(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return r
	}
	return p
}
